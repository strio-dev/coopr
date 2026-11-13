package buildah

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/identity"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/opencontainers/runtime-spec/specs-go"
	"github.com/strio-dev/coopr/internal/planner"
	"github.com/strio-dev/coopr/internal/stateidentity"
	upstream "go.podman.io/buildah"
	"go.podman.io/buildah/copier"
	"go.podman.io/storage"
	"go.podman.io/storage/pkg/idtools"
	"go.podman.io/storage/pkg/unshare"
)

// preparedRunInput keeps a filtered context snapshot alive through both the
// cache lookup and the RUN that consumes it. A hit and a miss therefore use
// the same context input identity.
type preparedRunInput struct {
	operation  Run
	contextDir string
	resolved   []instructionCacheResolvedInput
	complete   bool
	cleanup    func() error
}

func prepareBuildContext(ctx context.Context, plan *planner.Plan, options PlanOptions) (PlanOptions, func(error) error, error) {
	if options.ContextDir == "" || options.ContextPrepared || !planUsesWritableContext(plan, options.TransientRunMounts) {
		return options, func(err error) error { return err }, nil
	}
	return snapshotBuildContext(ctx, options)
}

func snapshotBuildContext(ctx context.Context, options PlanOptions) (PlanOptions, func(error) error, error) {
	options, snapshot, err := captureBuildContext(ctx, options, false)
	if err != nil {
		return options, nil, err
	}
	return options, func(err error) error { return RemoveWorkerDirectory(filepath.Dir(snapshot.directory), err) }, nil
}

func captureBuildContext(ctx context.Context, options PlanOptions, indexed bool) (PlanOptions, capturedContext, error) {
	if err := ctx.Err(); err != nil {
		return options, capturedContext{}, err
	}
	artifacts := append(slices.Clone(options.ContextArtifacts), options.Store.RunRoot, options.Store.GraphRoot, options.Store.ImageStore, options.Output.Path, options.CacheLocalDir, options.CacheMountDir)
	if options.ContextPrepared {
		// The retained ignore file already protects this disposable input tree.
		artifacts = nil
	}
	policy, err := prepareContextPolicyWithIgnore(options.ContextDir, artifacts, options.IgnoreFile)
	if err != nil {
		return options, capturedContext{}, err
	}
	contextOptions, err := policy.apply(upstream.AddAndCopyOptions{})
	if err != nil {
		return options, capturedContext{}, err
	}
	snapshot, err := captureContext(ctx, contextOptions.ContextDir, contextOptions.Excludes, nil, nil, indexed, options.workerDirectory)
	if err != nil {
		return options, capturedContext{}, err
	}
	// Retain the effective policy independently of filtered context files,
	// including protected output paths that RUN must not recreate for COPY.
	options.IgnoreFile = filepath.Join(filepath.Dir(snapshot.directory), "ignore")
	if err := os.WriteFile(options.IgnoreFile, []byte(strings.Join(contextOptions.Excludes, "\n")+"\n"), 0600); err != nil {
		return options, capturedContext{}, errors.Join(err, snapshot.cleanup())
	}
	options.ContextDir, options.ContextPrepared = snapshot.directory, true
	return options, snapshot, nil
}

func prepareRunInput(ctx context.Context, builder operationBuilder, contextDir string, artifacts []string, operation Run) (_ preparedRunInput, retErr error) {
	if err := ctx.Err(); err != nil {
		return preparedRunInput{}, err
	}
	operation.executionContext = ctx
	result := preparedRunInput{operation: operation, contextDir: contextDir, complete: true}
	defer func() {
		if retErr != nil && result.cleanup != nil {
			retErr = errors.Join(retErr, result.cleanup())
		}
	}()
	if operation.Stdin != nil {
		return result, nil // A streamed stdin has no stable cache identity.
	}
	if len(operation.Mounts) == 0 {
		return result, nil
	}
	if _, err := serializeRunMounts(operation.Mounts, contextDir); err != nil {
		return preparedRunInput{}, err
	}
	var contextIdentity string
	if hasContextBindMount(operation.Mounts) {
		if operation.contextPrepared {
			artifacts = nil
		}
		policy, err := prepareContextPolicyWithIgnore(contextDir, artifacts, operation.ContextIgnoreFile)
		if err != nil {
			return preparedRunInput{}, err
		}
		options, err := policy.apply(upstream.AddAndCopyOptions{})
		if err != nil {
			return preparedRunInput{}, err
		}
		uidMap, gidMap := contextSnapshotIDMaps(builder)
		if operation.contextPrepared {
			// Producer writes already carry parent-namespace ownership. Copy
			// those IDs unchanged rather than translating them a second time.
			uidMap, gidMap = nil, nil
		}
		// A prepared context can still be written by another stage. Retain an
		// operation-local version so hashing and execution consume the same
		// bytes, while subsequent operations can capture producer updates.
		snapshot, err := captureContext(ctx, options.ContextDir, options.Excludes, uidMap, gidMap, true, operation.workerDirectory)
		if err != nil {
			return preparedRunInput{}, err
		}
		result.contextDir, result.cleanup = snapshot.directory, snapshot.cleanup
		selected, err := snapshot.runMountIdentity(ctx, operation.Mounts)
		if err != nil {
			return preparedRunInput{}, err
		}
		contextIdentity = selected.String()
		result.operation.Mounts = append([]RunMount(nil), operation.Mounts...)
		for index := range result.operation.Mounts {
			if result.operation.Mounts[index].Type == "bind" && !result.operation.Mounts[index].BoundFrom && result.operation.Mounts[index].HostPath == "" {
				result.operation.Mounts[index].PrivateRelabel = !operation.contextPrepared
				result.operation.Mounts[index].SharedRelabel = operation.contextPrepared
			}
		}
	}
	result.resolved = make([]instructionCacheResolvedInput, 0, len(operation.Mounts))
	imageIdentities := make(map[string]string)
	result.operation.Mounts = slices.Clone(result.operation.Mounts)
	for index, mount := range operation.Mounts {
		var identity instructionCacheResolvedInput
		switch mount.Type {
		case "bind":
			if mount.Artifact != nil {
				root, cleanup, err := extractArtifactRunSource(ctx, operation.sourceStore, *mount.Artifact, operation.workerDirectory)
				if err != nil {
					return preparedRunInput{}, fmt.Errorf("prepare RUN artifact input: %w", err)
				}
				previous := result.cleanup
				result.cleanup = func() error {
					var prior error
					if previous != nil {
						prior = previous()
					}
					return errors.Join(prior, cleanup())
				}
				result.operation.Mounts[index].artifactRoot = root
				identity = instructionCacheResolvedInput{Kind: "artifact-files", Identity: mount.Artifact.Selected.Digest.String()}
			} else if mount.HostPath != "" {
				return preparedRunInput{}, errors.New("live host mounts are not cacheable")
			} else if mount.BoundFrom {
				imageID := mount.Properties["from"]
				rootDigest, found := imageIdentities[imageID]
				if !found {
					var err error
					rootDigest, err = runMountImageRootDigest(ctx, builder, operation.sourceStore, imageID)
					if err != nil {
						return preparedRunInput{}, fmt.Errorf("identify RUN bind source image: %w", err)
					}
					imageIdentities[imageID] = rootDigest
				}
				identity = instructionCacheResolvedInput{Kind: "image-rootfs", Identity: rootDigest}
			} else {
				identity = instructionCacheResolvedInput{Kind: "context", Identity: contextIdentity}
			}
		case "cache":
			from := mount.Properties["from"]
			if mount.BoundFrom {
				rootDigest, found := imageIdentities[from]
				if !found {
					var err error
					rootDigest, err = runMountImageRootDigest(ctx, builder, operation.sourceStore, from)
					if err != nil {
						return preparedRunInput{}, fmt.Errorf("identify RUN cache source image: %w", err)
					}
					imageIdentities[from] = rootDigest
				}
				from = rootDigest
			}
			identity = instructionCacheResolvedInput{Kind: "cache", Identity: mount.Properties["id"] + "\x00" + from}
		case "tmpfs":
			identity = instructionCacheResolvedInput{Kind: "tmpfs", Identity: "ephemeral"}
		case "secret", "ssh":
			// The operation itself carries the authored mount metadata. Keep only
			// a stable type marker here: secret bytes, SSH agent state, and the
			// host source selected by --secret/--ssh are session inputs and must
			// not invalidate an otherwise identical RUN cache key.
			identity = instructionCacheResolvedInput{Kind: mount.Type, Identity: "credential"}
		default:
			return preparedRunInput{}, fmt.Errorf("unsupported RUN cache mount type %q", mount.Type)
		}
		result.resolved = append(result.resolved, identity)
	}
	return result, nil
}

func runMountImageRootDigest(ctx context.Context, _ operationBuilder, store storage.Store, imageID string) (string, error) {
	if store == nil || imageID == "" {
		return "", errors.New("RUN mount has no resolved source image")
	}
	configData, err := packageImageConfig(ctx, store, imageID, nil)
	if err != nil {
		return "", fmt.Errorf("read RUN mount source image config: %w", err)
	}
	var image v1.Image
	if err := json.Unmarshal(configData, &image); err != nil {
		return "", fmt.Errorf("decode RUN mount source image config: %w", err)
	}
	return rootFSIdentity(image.RootFS)
}

func rootFSIdentity(rootFS v1.RootFS) (string, error) {
	if rootFS.Type != "layers" {
		return "", fmt.Errorf("image has unsupported rootfs type %q", rootFS.Type)
	}
	for index, diffID := range rootFS.DiffIDs {
		if err := diffID.Validate(); err != nil {
			return "", fmt.Errorf("image has invalid diff ID %d: %w", index, err)
		}
	}
	chainID := identity.ChainID(rootFS.DiffIDs)
	if chainID == "" {
		return "empty", nil
	}
	return chainID.String(), nil
}

type cacheContextWriter struct {
	ctx    context.Context
	writer io.Writer
}

func (writer cacheContextWriter) Write(data []byte) (int, error) {
	if err := writer.ctx.Err(); err != nil {
		return 0, err
	}
	return writer.writer.Write(data)
}

func applyRunWithFilteredContext(builder operationBuilder, contextDir string, artifacts []string, operation Run) (retErr error) {
	if !hasContextBindMount(operation.Mounts) || operation.contextPrepared {
		return operation.apply(builder, contextDir)
	}
	// Validate before touching the filesystem so malformed mount paths fail for
	// their authored reason instead of an incidental context snapshot error.
	if _, err := serializeRunMounts(operation.Mounts, contextDir); err != nil {
		return err
	}
	policy, err := prepareContextPolicyWithIgnore(contextDir, artifacts, operation.ContextIgnoreFile)
	if err != nil {
		return err
	}
	options, err := policy.apply(upstream.AddAndCopyOptions{})
	if err != nil {
		return err
	}
	uidMap, gidMap := contextSnapshotIDMaps(builder)
	ctx := operation.executionContext
	if ctx == nil {
		ctx = context.Background()
	}
	snapshot, cleanup, err := snapshotContext(ctx, options.ContextDir, options.Excludes, uidMap, gidMap, operation.workerDirectory)
	if err != nil {
		return err
	}
	defer func() {
		retErr = errors.Join(retErr, cleanup())
	}()
	operation.Mounts = append([]RunMount(nil), operation.Mounts...)
	for index := range operation.Mounts {
		if operation.Mounts[index].Type == "bind" && !operation.Mounts[index].BoundFrom {
			operation.Mounts[index].PrivateRelabel = true
		}
	}
	return operation.apply(builder, snapshot)
}

func hasContextBindMount(mounts []RunMount) bool {
	for _, mount := range mounts {
		if mount.Type == "bind" && !mount.BoundFrom && mount.HostPath == "" {
			return true
		}
	}
	return false
}

func snapshotContext(ctx context.Context, contextDir string, excludes []string, uidMap, gidMap []idtools.IDMap, parents ...string) (string, func() error, error) {
	capture, err := captureContext(ctx, contextDir, excludes, uidMap, gidMap, false, parents...)
	return capture.directory, capture.cleanup, err
}

func captureContext(ctx context.Context, contextDir string, excludes []string, uidMap, gidMap []idtools.IDMap, indexed bool, parents ...string) (capturedContext, error) {
	if err := ctx.Err(); err != nil {
		return capturedContext{}, err
	}
	if contextDir == "" {
		return capturedContext{}, errors.New("context snapshot requires a build context")
	}
	contextDir, err := filepath.Abs(contextDir)
	if err != nil {
		return capturedContext{}, fmt.Errorf("resolve context snapshot location: %w", err)
	}
	contextDir, err = filepath.EvalSymlinks(contextDir)
	if err != nil {
		return capturedContext{}, fmt.Errorf("resolve context snapshot root: %w", err)
	}
	temporaryParent := ""
	if len(parents) == 1 {
		temporaryParent = parents[0]
	}
	temporaryRoot, err := os.MkdirTemp(temporaryParent, "coopr-context-")
	if err != nil {
		return capturedContext{}, fmt.Errorf("create context snapshot root: %w", err)
	}
	cleanup := func() error {
		if err := os.RemoveAll(temporaryRoot); err != nil {
			return fmt.Errorf("remove context snapshot: %w", err)
		}
		return nil
	}
	snapshot := filepath.Join(temporaryRoot, "context")
	if err := os.Mkdir(snapshot, 0700); err != nil {
		_ = cleanup()
		return capturedContext{}, fmt.Errorf("create context snapshot: %w", err)
	}

	reader, writer := io.Pipe()
	stopCancellation := context.AfterFunc(ctx, func() {
		_ = reader.CloseWithError(ctx.Err())
		_ = writer.CloseWithError(ctx.Err())
	})
	defer stopCancellation()
	getResult := make(chan error, 1)
	input := io.Reader(reader)
	var finishObserver func(error) (map[string]contextPayload, error)
	if indexed {
		input, finishObserver = observeContextPayloads(ctx, input, contextDir)
	}
	go func() {
		getErr := copier.Get(contextDir, contextDir, copier.GetOptions{
			Excludes: excludes, KeepDirectoryNames: true,
		}, []string{"."}, cacheContextWriter{ctx: ctx, writer: writer})
		getResult <- errors.Join(getErr, writer.CloseWithError(getErr))
	}()
	putErr := copier.Put(snapshot, snapshot, copier.PutOptions{
		UIDMap: uidMap, GIDMap: gidMap, IgnoreDevices: unshare.IsRootless(),
		// Keep the native root entry so Put applies its complete metadata and
		// ID mappings. Rename only here: Get does not rename hardlink targets.
		// A chrooted Get names its root "/"; an unprivileged Get uses basename.
		Rename: map[string]string{filepath.Base(filepath.Clean(contextDir)): ".", "/": "."},
	}, input)
	if putErr != nil {
		_ = reader.CloseWithError(putErr)
	} else {
		_ = reader.Close()
	}
	getErr := <-getResult
	var payloads map[string]contextPayload
	var observerErr error
	if finishObserver != nil {
		payloads, observerErr = finishObserver(putErr)
	}
	if err := errors.Join(ctx.Err(), getErr, putErr, observerErr); err != nil {
		_ = cleanup()
		return capturedContext{}, fmt.Errorf("snapshot build context: %w", err)
	}
	var identity digest.Digest
	var index *stateidentity.CapturedIndex
	if indexed {
		index, err = indexCapturedContext(ctx, snapshot, payloads)
		if err != nil {
			return capturedContext{}, errors.Join(fmt.Errorf("index captured context: %w", err), cleanup())
		}
		identity = index.Digest()
	}
	return capturedContext{directory: snapshot, identity: identity, index: index, cleanup: cleanup}, nil
}

func contextSnapshotIDMaps(builder operationBuilder) ([]idtools.IDMap, []idtools.IDMap) {
	native, ok := builder.(nativeBuilder)
	if !ok || native.Builder == nil {
		return nil, nil
	}
	convert := func(mappings []specs.LinuxIDMapping) []idtools.IDMap {
		result := make([]idtools.IDMap, len(mappings))
		for index, mapping := range mappings {
			result[index] = idtools.IDMap{ContainerID: int(mapping.ContainerID), HostID: int(mapping.HostID), Size: int(mapping.Size)}
		}
		return result
	}
	return convert(native.IDMappingOptions.UIDMap), convert(native.IDMappingOptions.GIDMap)
}

// Writable binds into the primary context are build-scoped side effects. An
// instruction image cache cannot replay them, so those RUNs must execute.
func hasWritableContextBindMount(mounts []RunMount) bool {
	for _, mount := range mounts {
		if mount.Type == "bind" && !mount.BoundFrom && mount.HostPath == "" && mount.Properties["from"] == "" {
			properties, err := normalizeRunMountProperties(mount.Type, mount.Properties)
			if err == nil && properties["readonly"] == "false" {
				return true
			}
		}
	}
	return false
}

func planUsesWritableContext(plan *planner.Plan, mounts []RunMount) bool {
	if hasWritableContextBindMount(mounts) {
		return true
	}
	for _, stage := range plan.Stages {
		if stage.DeferredImageSource {
			return true
		}
		for _, operation := range stage.Operations {
			if operation.Name == "component" {
				return true
			}
			if operation.Name != "run" {
				continue
			}
			for index, child := range operation.Children {
				if operation.HostMounts[index] != "" {
					continue
				}
				if child.Name == "mount" && len(child.Arguments) == 1 && hasWritableContextBindMount([]RunMount{{Type: child.Arguments[0], Properties: child.Properties}}) {
					return true
				}
			}
		}
	}
	return false
}

func writableContextBindOptions(properties map[string]string) []string {
	// Mount admission accepts all native ParseBool spellings. Preserve that
	// interpretation when translating validated properties into direct mounts.
	enabled := func(name string) bool {
		value, _ := strconv.ParseBool(properties[name])
		return value
	}
	options := []string{"rw"}
	if enabled("bind-nonrecursive") {
		options = append(options, "bind")
	} else {
		options = append(options, "rbind")
	}
	relabel := "z"
	if properties["relabel"] == "private" {
		relabel = "Z"
	}
	options = append(options, relabel)
	for _, name := range []string{"nosuid", "nodev", "noexec", "shared", "rshared", "private", "rprivate", "slave", "rslave", "no-dereference"} {
		if enabled(name) {
			options = append(options, name)
		}
	}
	if enabled("u") {
		options = append(options, "U")
	}
	if propagation := properties["bind-propagation"]; propagation != "" {
		options = append(options, propagation)
	}
	return options
}
