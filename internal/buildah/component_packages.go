package buildah

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"coopr/internal/imageconfig"
	"coopr/internal/oci"

	"github.com/containerd/platforms"
	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"go.podman.io/image/v5/types"
	"go.podman.io/storage"
)

// ImportedComponentPackage is a package snapshot ready to use as a Buildah
// stage base. Config is independent for every caller because stage execution
// mutates its logical image configuration.
type ImportedComponentPackage struct {
	ImageID        string
	ManifestDigest digest.Digest
	Config         *imageconfig.Config
}

// ComponentPackageImporter owns package imports for one build. It coalesces
// concurrent requests for identical immutable package inputs and retains only
// the imported image identity and executor-neutral configuration sidecar.
type ComponentPackageImporter struct {
	resolver *oci.Resolver
	store    storage.Store
	system   *types.SystemContext
	tempDir  string

	mu      sync.Mutex
	imports map[componentPackageKey]*componentPackageImport
}

type componentPackageKey struct {
	componentDigest digest.Digest
	packageMedia    string
	packageDigest   digest.Digest
	packageSize     int64
	configDigest    digest.Digest
	platform        string
}

type componentPackageImport struct {
	done   chan struct{}
	result ImportedComponentPackage
	err    error
}

// NewComponentPackageImporter creates a package importer scoped to an existing
// Buildah containers/storage store. tempDir is the parent for disposable
// verified downloads; an empty value uses the process temporary directory.
func NewComponentPackageImporter(resolver *oci.Resolver, store storage.Store, system *types.SystemContext, tempDir string) (*ComponentPackageImporter, error) {
	if resolver == nil {
		return nil, errors.New("component package resolver is required")
	}
	if store == nil {
		return nil, errors.New("component package store is required")
	}
	if tempDir == "" {
		tempDir = os.TempDir()
	}
	if !filepath.IsAbs(tempDir) {
		return nil, errors.New("component package temporary directory must be absolute")
	}
	return &ComponentPackageImporter{
		resolver: resolver, store: store, system: system, tempDir: tempDir,
		imports: make(map[componentPackageKey]*componentPackageImport),
	}, nil
}

// Import downloads and imports the verified package bound to stageID. The
// resolved component plan and descriptor remain the authority for reachability
// and content identity; temporary downloads are always removed before return.
func (importer *ComponentPackageImporter) Import(ctx context.Context, resolved *ResolvedComponentPlan, stageID string, platform v1.Platform) (ImportedComponentPackage, error) {
	if importer == nil {
		return ImportedComponentPackage{}, errors.New("component package importer is nil")
	}
	if ctx == nil {
		return ImportedComponentPackage{}, errors.New("component package import context is nil")
	}
	if err := ctx.Err(); err != nil {
		return ImportedComponentPackage{}, err
	}
	if resolved == nil || resolved.Artifact == nil {
		return ImportedComponentPackage{}, errors.New("resolved component artifact is required")
	}
	if stageID == "" {
		return ImportedComponentPackage{}, errors.New("component package stage ID is required")
	}
	if resolved.Identity.Algorithm() != digest.SHA256 || resolved.Identity.Validate() != nil || resolved.Artifact.Selected.Digest != resolved.Identity {
		return ImportedComponentPackage{}, errors.New("resolved component identity does not match its selected artifact")
	}
	pkg, ok := resolved.PackageInputs[stageID]
	if !ok {
		return ImportedComponentPackage{}, fmt.Errorf("component package stage %q is not bound", stageID)
	}
	key, err := componentPackageCacheKey(resolved.Identity, pkg, platform)
	if err != nil {
		return ImportedComponentPackage{}, err
	}

	importer.mu.Lock()
	entry, exists := importer.imports[key]
	if !exists {
		entry = &componentPackageImport{done: make(chan struct{})}
		importer.imports[key] = entry
	}
	importer.mu.Unlock()
	if exists {
		select {
		case <-ctx.Done():
			return ImportedComponentPackage{}, ctx.Err()
		case <-entry.done:
			if entry.err != nil {
				return ImportedComponentPackage{}, entry.err
			}
			return cloneImportedComponentPackage(entry.result), entry.err
		}
	}

	entry.result, entry.err = importer.importPackage(ctx, resolved, pkg, platform)
	importer.mu.Lock()
	if entry.err != nil {
		delete(importer.imports, key)
	}
	close(entry.done)
	importer.mu.Unlock()
	if entry.err != nil {
		return ImportedComponentPackage{}, entry.err
	}
	return cloneImportedComponentPackage(entry.result), entry.err
}

func (importer *ComponentPackageImporter) importPackage(ctx context.Context, resolved *ResolvedComponentPlan, pkg oci.Package, platform v1.Platform) (ImportedComponentPackage, error) {
	downloadDir, err := os.MkdirTemp(importer.tempDir, "coopr-component-package-")
	if err != nil {
		return ImportedComponentPackage{}, fmt.Errorf("create component package download directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(downloadDir) }()
	download := filepath.Join(downloadDir, "package.tar")
	if err := importer.resolver.Download(ctx, resolved.Artifact, pkg.Descriptor, download); err != nil {
		return ImportedComponentPackage{}, fmt.Errorf("download component package %q: %w", pkg.Stage, err)
	}
	imageID, rawConfig, err := ImportPackageSnapshot(ctx, importer.store, importer.system, pkg, download, platform)
	if err != nil {
		return ImportedComponentPackage{}, err
	}
	config, err := imageconfig.Parse(rawConfig)
	if err != nil {
		return ImportedComponentPackage{}, fmt.Errorf("parse imported component package %q config: %w", pkg.Stage, err)
	}
	manifestDigest, err := componentPackageImageManifestDigest(pkg, rawConfig)
	if err != nil {
		return ImportedComponentPackage{}, fmt.Errorf("identify imported component package %q manifest: %w", pkg.Stage, err)
	}
	return ImportedComponentPackage{ImageID: imageID, ManifestDigest: manifestDigest, Config: config}, nil
}

func componentPackageImageManifestDigest(pkg oci.Package, configData []byte) (digest.Digest, error) {
	layer := pkg.Descriptor
	layer.MediaType = v1.MediaTypeImageLayer
	config := oci.Descriptor(v1.MediaTypeImageConfig, configData)
	manifestData, err := json.Marshal(v1.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: v1.MediaTypeImageManifest,
		Config:    config,
		Layers:    []v1.Descriptor{layer},
	})
	if err != nil {
		return "", err
	}
	return digest.FromBytes(manifestData), nil
}

func componentPackagePlatformKey(platform v1.Platform) (string, error) {
	if _, err := componentPlatformString(platform); err != nil {
		return "", err
	}
	return platforms.Format(platforms.Normalize(platform)), nil
}

func componentPackageCacheKey(component digest.Digest, pkg oci.Package, platform v1.Platform) (componentPackageKey, error) {
	platformKey, err := componentPackagePlatformKey(platform)
	if err != nil {
		return componentPackageKey{}, err
	}
	return componentPackageKey{
		componentDigest: component,
		packageMedia:    pkg.Descriptor.MediaType,
		packageDigest:   pkg.Descriptor.Digest,
		packageSize:     pkg.Descriptor.Size,
		configDigest:    digest.FromBytes(pkg.Config),
		platform:        platformKey,
	}, nil
}

func cloneImportedComponentPackage(result ImportedComponentPackage) ImportedComponentPackage {
	return ImportedComponentPackage{ImageID: result.ImageID, ManifestDigest: result.ManifestDigest, Config: result.Config.Clone()}
}
