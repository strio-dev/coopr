package buildah

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"

	"coopr/internal/imageconfig"
	"coopr/internal/oci"
	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	upstream "go.podman.io/buildah"
	"go.podman.io/buildah/define"
)

// finalizeOutput runs after the normal instruction-cache path, including cache
// hits and unchanged bases. Output choices never change instruction cache keys.
func (executor *graphExecutor) finalizeOutput(ctx context.Context, result Result, baseID string, builderOptions upstream.BuilderOptions, logical *imageconfig.Config) (_ Result, retErr error) {
	output := executor.options.Output
	unsquashedLayout := result.Layout
	filesystems := append([]FilesystemOutput(nil), output.Filesystems...)
	if output.Filesystem.Path != "" {
		filesystems = append([]FilesystemOutput{output.Filesystem}, filesystems...)
	}
	if !output.Squash && !output.SquashAll && len(output.SBOM) == 0 && len(filesystems) == 0 && !output.ConfidentialWorkload.Convert {
		if err := executor.retainResultLayers(result); err != nil {
			return Result{}, err
		}
		return result, nil
	}
	squashFilesystemNoop := false
	if output.Squash && !output.SquashAll && len(output.SBOM) == 0 {
		baseImage, err := executor.store.Image(baseID)
		if err != nil {
			return Result{}, fmt.Errorf("inspect squash base image: %w", err)
		}
		resultImage, err := executor.store.Image(result.ImageID)
		if err != nil {
			return Result{}, fmt.Errorf("inspect squash result image: %w", err)
		}
		squashFilesystemNoop = baseImage.TopLayer == resultImage.TopLayer
	}
	finalOptions := builderOptions
	finalOptions.FromImage = result.ImageID
	finalOptions.Container = ""
	if output.Squash && !output.SquashAll && len(output.SBOM) == 0 && !squashFilesystemNoop {
		finalOptions.FromImage = baseID
	}
	builder, err := upstream.NewBuilder(ctx, executor.store, finalOptions)
	if err != nil {
		return Result{}, fmt.Errorf("open final image: %w", err)
	}
	defer func() {
		if builder != nil {
			retErr = errors.Join(retErr, builder.Delete())
		}
	}()
	if output.Squash && !output.SquashAll && len(output.SBOM) == 0 && !squashFilesystemNoop {
		if err := ApplyComponentDelta(ctx, executor.store, baseID, result.ImageID, builder, os.TempDir()); err != nil {
			return Result{}, fmt.Errorf("squash new layers: %w", err)
		}
		if !executor.options.ImageControls.OmitHistory {
			if err := preserveSquashedHistory(builder, logical); err != nil {
				return Result{}, err
			}
		}
	}
	if err := syncBuilderConfig(builder, logical); err != nil {
		return Result{}, err
	}
	if output.Squash && !squashFilesystemNoop || output.SquashAll || len(output.SBOM) != 0 || output.ConfidentialWorkload.Convert {
		scans := slices.Clone(output.SBOM)
		var cleanup func() error
		if len(scans) != 0 {
			artifacts := executor.options.ContextArtifacts
			if executor.options.ContextPrepared {
				artifacts = nil
			}
			policy, err := prepareContextPolicyWithIgnore(executor.options.ContextDir, artifacts, executor.options.IgnoreFile)
			if err != nil {
				return Result{}, err
			}
			copyOptions, err := policy.apply(upstream.AddAndCopyOptions{})
			if err != nil {
				return Result{}, err
			}
			uidMap, gidMap := contextSnapshotIDMaps(nativeBuilder{Builder: builder})
			contextDir, remove, err := snapshotContext(copyOptions.ContextDir, copyOptions.Excludes, uidMap, gidMap)
			if err != nil {
				return Result{}, err
			}
			cleanup = remove
			defer func() { retErr = errors.Join(retErr, cleanup()) }()
			for i := range scans {
				resolved, err := executor.resolveBaseImage(ctx, scans[i].Image, v1.Platform{OS: "linux", Architecture: runtime.GOARCH})
				if err != nil {
					return Result{}, fmt.Errorf("resolve SBOM scanner %s: %w", scans[i].Image, err)
				}
				scans[i].Image = resolved.ImageID
				scans[i].PullPolicy = define.PullNever
				scans[i].ContextDir = append([]string{contextDir}, scans[i].ContextDir...)
			}
		}
		commitOptions := upstream.CommitOptions{
			PreferredManifestType: builderOptions.Format,
			EmptyLayerIfEmptyDiff: keepEmptyFilesystemLayer(builderOptions.Format),
			Squash:                output.SquashAll,
			OmitHistory:           executor.options.ImageControls.OmitHistory,
			OmitLayerHistoryEntry: len(scans) != 0,
			SBOMScanOptions:       scans,
			SystemContext:         builderOptions.SystemContext,
		}
		// Podman/Buildah scan on the final instruction commit, whose writable
		// layer already contains that instruction's diff. Coopr scans from the
		// committed instruction image, so omit this empty writable layer while
		// retaining Buildah's synthesized embedded-SBOM layer.
		if err := applyFinalCommitOptions(&commitOptions, output); err != nil {
			return Result{}, err
		}
		commitOptions.ConfidentialWorkloadOptions = output.ConfidentialWorkload
		if len(scans) != 0 {
			// Scanner programs run on the host while inspecting the target's
			// rootfs as data; they do not require target-architecture emulation.
			commitOptions.SystemContext = platformSystemContext(builderOptions.SystemContext, v1.Platform{OS: "linux", Architecture: runtime.GOARCH})
			builder.SetCreatedBy("coopr SBOM scan")
			if output.Squash && !output.SquashAll {
				// This image is only a delta source. Give it private provenance
				// so deleting it cannot race another build sharing a config ID.
				builder.SetCreatedBy("coopr temporary SBOM scan " + builder.ContainerID)
			}
		} else if output.Squash || output.SquashAll {
			builder.SetCreatedBy("coopr squash")
		}
		timestampPolicyFromOptions(executor.options).apply(&commitOptions)
		imageID, _, manifestDigest, err := builder.Commit(ctx, nil, commitOptions)
		if output.Squash && !output.SquashAll && len(scans) != 0 && imageID != "" {
			scannedImageID := imageID
			defer func() {
				_, err := executor.store.DeleteImage(scannedImageID, true)
				retErr = errors.Join(retErr, err)
			}()
		}
		if err != nil {
			return Result{}, fmt.Errorf("finalize image: %w", err)
		}
		if output.Squash && !output.SquashAll && len(scans) != 0 {
			// Buildah adds embedded scan files as another layer. Compact the
			// scanned image's complete delta so --squash still adds one layer.
			if err := builder.Delete(); err != nil {
				return Result{}, err
			}
			finalOptions.FromImage = baseID
			builder, err = upstream.NewBuilder(ctx, executor.store, finalOptions)
			if err != nil {
				return Result{}, err
			}
			if err := ApplyComponentDelta(ctx, executor.store, baseID, imageID, builder, os.TempDir()); err != nil {
				return Result{}, err
			}
			if err := syncBuilderConfig(builder, logical); err != nil {
				return Result{}, err
			}
			if !executor.options.ImageControls.OmitHistory {
				if err := preserveSquashedHistory(builder, logical); err != nil {
					return Result{}, err
				}
			}
			builder.SetCreatedBy("coopr squash")
			commitOptions.SBOMScanOptions = nil
			commitOptions.OmitLayerHistoryEntry = false
			commitOptions.SystemContext = builderOptions.SystemContext
			imageID, _, manifestDigest, err = builder.Commit(ctx, nil, commitOptions)
			if err != nil {
				return Result{}, fmt.Errorf("squash scanned image: %w", err)
			}
		}
		staging, err := os.MkdirTemp(filepath.Dir(output.Path), ".coopr-finalized-*")
		if err != nil {
			return Result{}, err
		}
		defer func() { retErr = errors.Join(retErr, os.RemoveAll(staging)) }()
		finalOutput := output
		finalOutput.Path = filepath.Join(staging, "layout")
		result, err = copyStoredOutputSelected(ctx, executor.store, imageID, finalOutput, builderOptions.SystemContext, &manifestDigest)
		if err != nil {
			return Result{}, err
		}
		finalLogical := logical
		if output.ConfidentialWorkload.Convert {
			raw, err := oci.ReadImageConfigLayout(ctx, result.Layout)
			if err != nil {
				return Result{}, fmt.Errorf("read confidential workload image config: %w", err)
			}
			finalLogical, err = imageconfig.Parse(raw)
			if err != nil {
				return Result{}, fmt.Errorf("parse confidential workload image config: %w", err)
			}
		}
		result, err = executor.reconcileOutputConfig(ctx, result, builderOptions.SystemContext, finalLogical, false)
		if err != nil {
			return Result{}, err
		}
		if output.Squash && !output.SquashAll {
			restored, err := restoreSquashBaseLayers(ctx, unsquashedLayout, result.Layout)
			if err != nil {
				return Result{}, fmt.Errorf("restore exact squash base layers: %w", err)
			}
			restoredImageID, err := ImportSelectedImage(ctx, executor.store, builderOptions.SystemContext, result.Layout, restored)
			if err != nil {
				return Result{}, fmt.Errorf("retain exact squash manifest: %w", err)
			}
			if restoredImageID != result.ImageID {
				return Result{}, fmt.Errorf("restored squash image ID %s differs from committed image ID %s", restoredImageID, result.ImageID)
			}
			result.ManifestDigest = restored.Digest.String()
		}
		if err := os.RemoveAll(output.Path); err != nil {
			return Result{}, err
		}
		if err := os.Rename(result.Layout, output.Path); err != nil {
			return Result{}, err
		}
		result.Layout = output.Path
		// Embedded SBOM files exist in the committed image, not the scanner's
		// original builder. Export from that exact final image.
		if len(filesystems) != 0 {
			if err := builder.Delete(); err != nil {
				return Result{}, err
			}
			finalOptions.FromImage = result.ImageID
			builder, err = upstream.NewBuilder(ctx, executor.store, finalOptions)
			if err != nil {
				return Result{}, err
			}
		}
	}
	// VOLUME is a metadata instruction in imagebuildah, but its working
	// filesystem includes the declared directories. Recreate them in this
	// export-only builder after reopening the committed image.
	if len(filesystems) != 0 {
		adapter := executor.nativeBuilder(builder, finalOptions)
		for _, volume := range builder.Volumes() {
			if err := adapter.ensureContainerPathIsDirectory(volume, "0"); err != nil {
				return Result{}, err
			}
		}
	}
	for _, filesystem := range filesystems {
		if err := exportFilesystem(ctx, builder, filesystem, timestampPolicyFromOptions(executor.options)); err != nil {
			return Result{}, err
		}
	}
	if err := executor.retainResultLayers(result); err != nil {
		return Result{}, err
	}
	return result, nil
}

func (executor *graphExecutor) retainResultLayers(result Result) error {
	manifestDigest, err := digest.Parse(result.ManifestDigest)
	if err != nil {
		return fmt.Errorf("parse final manifest digest: %w", err)
	}
	return retainStoredLayoutLayers(executor.store, result.ImageID, result.Layout, manifestDigest)
}

func preserveSquashedHistory(builder *upstream.Builder, logical *imageconfig.Config) error {
	raw, err := logical.MarshalJSON()
	if err != nil {
		return err
	}
	var image struct {
		History []v1.History `json:"history"`
	}
	if err := json.Unmarshal(raw, &image); err != nil {
		return err
	}
	baseHistory := len(builder.OCIv1.History)
	if baseHistory > len(image.History) {
		return errors.New("squash base history exceeds final image history")
	}
	for _, entry := range image.History[baseHistory:] {
		builder.AddPrependedEmptyLayer(entry.Created, entry.CreatedBy, entry.Author, entry.Comment)
	}
	return nil
}

func validateFinalizationOutput(output Output) error {
	if output.Squash && output.SquashAll {
		return errors.New("--squash and --squash-all are mutually exclusive")
	}
	filesystems := append([]FilesystemOutput(nil), output.Filesystems...)
	if output.Filesystem.Path != "" {
		filesystems = append([]FilesystemOutput{output.Filesystem}, filesystems...)
	}
	for _, filesystem := range filesystems {
		if filesystem.Type != "local" && filesystem.Type != "tar" {
			return errors.New("filesystem output must be local or tar")
		}
		if !filepath.IsAbs(filesystem.Path) {
			return errors.New("worker filesystem output path must be absolute")
		}
	}
	for _, scan := range output.SBOM {
		if scan.Image == "" || len(scan.Commands) == 0 {
			return errors.New("SBOM scan requires a scanner image and command")
		}
		if scan.SBOMOutput == "" && scan.PURLOutput == "" && scan.ImageSBOMOutput == "" && scan.ImagePURLOutput == "" {
			return errors.New("SBOM scan requires an output path")
		}
		for _, path := range []string{scan.SBOMOutput, scan.PURLOutput} {
			if path != "" && !filepath.IsAbs(path) {
				return errors.New("worker SBOM output path must be absolute")
			}
		}
		switch scan.MergeStrategy {
		case define.SBOMMergeStrategyCat, define.SBOMMergeStrategyCycloneDXByComponentNameAndVersion, define.SBOMMergeStrategySPDXByPackageNameAndVersionInfo:
		default:
			return fmt.Errorf("unknown SBOM merge strategy %q", scan.MergeStrategy)
		}
	}
	return nil
}
