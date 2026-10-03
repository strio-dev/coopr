package oci

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content"
	orasoci "oras.land/oras-go/v2/content/oci"
)

// PublishComponent publishes a complete single-platform component. packagePaths
// maps every retained package stage to an existing immutable snapshot tar.
// The mutable target tag is changed only after all content has been validated
// and uploaded. Its digest can be used to construct an immutable reference.
func (r *Resolver) PublishComponent(ctx context.Context, target string, meta ComponentMetadata, packagePaths map[string]string) (v1.Descriptor, error) {
	ref, err := ParseReference(target)
	if err != nil {
		return v1.Descriptor{}, err
	}
	repo, err := r.repository(ref)
	if err != nil {
		return v1.Descriptor{}, err
	}
	root, err := writeComponent(ctx, repo, meta, packagePaths)
	if err != nil {
		return v1.Descriptor{}, err
	}
	if err := checkTargetDigest(ref.Reference, root.Digest); err != nil {
		return v1.Descriptor{}, err
	}
	if err := ctx.Err(); err != nil {
		return v1.Descriptor{}, err
	}
	if _, isDigest := ref.Digest(); isDigest != nil {
		if err := repo.Tag(ctx, root, ref.Reference); err != nil {
			return v1.Descriptor{}, fmt.Errorf("tag component: %w", err)
		}
	}
	return root, nil
}

// WriteComponentLayout creates a complete component artifact in an OCI image
// layout without changing any remote or shared local reference.
func WriteComponentLayout(ctx context.Context, layout string, meta ComponentMetadata, packagePaths map[string]string) (v1.Descriptor, error) {
	if layout == "" {
		return v1.Descriptor{}, errors.New("empty component OCI layout path")
	}
	target, err := orasoci.NewWithContext(ctx, layout)
	if err != nil {
		return v1.Descriptor{}, err
	}
	root, err := writeComponent(ctx, target, meta, packagePaths)
	if err != nil {
		return v1.Descriptor{}, err
	}
	if err := target.Tag(ctx, root, root.Digest.String()); err != nil {
		return v1.Descriptor{}, fmt.Errorf("anchor component root: %w", err)
	}
	return root, nil
}

func writeComponent(ctx context.Context, target oras.Target, meta ComponentMetadata, packagePaths map[string]string) (v1.Descriptor, error) {
	if err := checkPlatform(meta.Platform); err != nil {
		return v1.Descriptor{}, err
	}
	layers := make([]v1.Descriptor, len(meta.Packages))
	for i, pkg := range meta.Packages {
		layers[i] = pkg.Descriptor
	}
	if err := validateComponent(meta, layers, meta.Platform); err != nil {
		return v1.Descriptor{}, err
	}
	if len(packagePaths) != len(meta.Packages) {
		return v1.Descriptor{}, fmt.Errorf("package source count differs from component metadata")
	}
	for _, pkg := range meta.Packages {
		path, ok := packagePaths[pkg.Stage]
		if !ok || path == "" {
			return v1.Descriptor{}, fmt.Errorf("missing package source %q", pkg.Stage)
		}
		if err := verifyFile(ctx, path, pkg.Descriptor); err != nil {
			return v1.Descriptor{}, fmt.Errorf("package %q: %w", pkg.Stage, err)
		}
		var image v1.Image
		if err := json.Unmarshal(pkg.Config, &image); err != nil || !platformEqual(image.Platform, meta.Platform) || image.RootFS.Type != "layers" || len(image.RootFS.DiffIDs) != 1 || image.RootFS.DiffIDs[0] != pkg.Descriptor.Digest {
			return v1.Descriptor{}, fmt.Errorf("package %q configuration does not describe its snapshot and platform: %v", pkg.Stage, err)
		}
	}
	configBytes, err := json.Marshal(meta)
	if err != nil {
		return v1.Descriptor{}, fmt.Errorf("marshal component metadata: %w", err)
	}
	if len(configBytes) > maxMetadataBytes {
		return v1.Descriptor{}, fmt.Errorf("component metadata exceeds %d bytes", maxMetadataBytes)
	}
	config := Descriptor(ComponentConfigType, configBytes)
	manifestBytes, err := json.Marshal(VersionedManifest(config, layers, ComponentArtifactType))
	if err != nil {
		return v1.Descriptor{}, fmt.Errorf("marshal component manifest: %w", err)
	}
	root := Descriptor(v1.MediaTypeImageManifest, manifestBytes)
	for _, pkg := range meta.Packages {
		file, err := os.Open(packagePaths[pkg.Stage])
		if err != nil {
			return v1.Descriptor{}, err
		}
		pushErr := target.Push(ctx, pkg.Descriptor, file)
		_ = file.Close() // ORAS may close it; cover failures before ORAS takes ownership.
		if pushErr != nil {
			return v1.Descriptor{}, fmt.Errorf("push package %q: %w", pkg.Stage, pushErr)
		}
	}
	if err := target.Push(ctx, config, bytes.NewReader(configBytes)); err != nil {
		return v1.Descriptor{}, fmt.Errorf("push component config: %w", err)
	}
	if err := target.Push(ctx, root, bytes.NewReader(manifestBytes)); err != nil {
		return v1.Descriptor{}, fmt.Errorf("push component manifest: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return v1.Descriptor{}, err
	}
	return root, nil
}

// PublishLayout copies an already-built OCI graph to a registry and changes a
// mutable tag only after the complete graph has been uploaded.
func (r *Resolver) PublishLayout(ctx context.Context, target, layout string, root v1.Descriptor) (string, error) {
	ref, err := ParseReference(target)
	if err != nil {
		return "", err
	}
	if err := checkTargetDigest(ref.Reference, root.Digest); err != nil {
		return "", err
	}
	source, err := orasoci.NewWithContext(ctx, layout)
	if err != nil {
		return "", err
	}
	resolved, err := source.Resolve(ctx, root.Digest.String())
	if err != nil || !sameDescriptor(resolved, root) {
		return "", fmt.Errorf("OCI layout root is missing or differs: %v", err)
	}
	if err := verifyArchiveGraph(ctx, source, root); err != nil {
		return "", fmt.Errorf("invalid OCI graph: %w", err)
	}
	repo, err := r.repository(ref)
	if err != nil {
		return "", err
	}
	if err := oras.CopyGraph(ctx, source, repo, root, oras.CopyGraphOptions{}); err != nil {
		return "", fmt.Errorf("copy OCI graph: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if _, isDigest := ref.Digest(); isDigest != nil {
		if err := repo.Tag(ctx, root, ref.Reference); err != nil {
			return "", fmt.Errorf("tag OCI graph: %w", err)
		}
	}
	return ref.Registry + "/" + ref.Repository + "@" + root.Digest.String(), nil
}

func sameDescriptor(a, b v1.Descriptor) bool {
	return a.MediaType == b.MediaType && a.Digest == b.Digest && a.Size == b.Size
}

// PublishImageArchive transfers the sole root image from a standard OCI archive
// without unpacking or rewriting its layers or configuration.
func (r *Resolver) PublishImageArchive(ctx context.Context, target, archivePath string) (v1.Descriptor, error) {
	ref, err := ParseReference(target)
	if err != nil {
		return v1.Descriptor{}, err
	}
	root, err := archiveRoot(archivePath)
	if err != nil {
		return v1.Descriptor{}, err
	}
	if err := checkTargetDigest(ref.Reference, root.Digest); err != nil {
		return v1.Descriptor{}, err
	}
	source, err := orasoci.NewFromTar(ctx, archivePath)
	if err != nil {
		return v1.Descriptor{}, err
	}
	resolved, err := source.Resolve(ctx, root.Digest.String())
	if err != nil || resolved.Digest != root.Digest || resolved.Size != root.Size {
		return v1.Descriptor{}, fmt.Errorf("archive root is missing or differs from index: %v", err)
	}
	if err := verifyArchiveGraph(ctx, source, root); err != nil {
		return v1.Descriptor{}, fmt.Errorf("invalid OCI archive graph: %w", err)
	}
	repo, err := r.repository(ref)
	if err != nil {
		return v1.Descriptor{}, err
	}
	if err := oras.CopyGraph(ctx, source, repo, root, oras.CopyGraphOptions{}); err != nil {
		return v1.Descriptor{}, fmt.Errorf("copy OCI image graph: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return v1.Descriptor{}, err
	}
	if _, isDigest := ref.Digest(); isDigest != nil {
		if err := repo.Tag(ctx, root, ref.Reference); err != nil {
			return v1.Descriptor{}, fmt.Errorf("tag image: %w", err)
		}
	}
	return root, nil
}

func verifyArchiveGraph(ctx context.Context, source content.ReadOnlyStorage, root v1.Descriptor) error {
	pending := []v1.Descriptor{root}
	seen := make(map[string]bool)
	for len(pending) != 0 {
		desc := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		key := fmt.Sprintf("%s/%s/%d", desc.Digest, desc.MediaType, desc.Size)
		if seen[key] {
			continue
		}
		seen[key] = true
		if err := checkDescriptor(desc); err != nil {
			return err
		}
		if err := verifySourceContent(ctx, source, desc); err != nil {
			return fmt.Errorf("%s: %w", desc.Digest, err)
		}
		next, err := content.Successors(ctx, source, desc)
		if err != nil {
			return err
		}
		pending = append(pending, next...)
	}
	return nil
}

func verifySourceContent(ctx context.Context, source content.ReadOnlyStorage, desc v1.Descriptor) error {
	stream, err := source.Fetch(ctx, desc)
	if err != nil {
		return err
	}
	defer func() { _ = stream.Close() }()
	verified := content.NewVerifyReader(stream, desc)
	if _, err := io.Copy(io.Discard, &contextReader{ctx: ctx, reader: verified}); err != nil {
		return err
	}
	return verified.Verify()
}

func checkTargetDigest(reference string, actual digest.Digest) error {
	if pinned := digest.Digest(reference); pinned.Validate() == nil && pinned != actual {
		return fmt.Errorf("target digest %s differs from publication %s", pinned, actual)
	}
	return nil
}

func verifyFile(ctx context.Context, path string, desc v1.Descriptor) error {
	if err := checkDescriptor(desc); err != nil {
		return err
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() != desc.Size {
		return fmt.Errorf("source is not a regular file of expected size %d", desc.Size)
	}
	verified := content.NewVerifyReader(file, desc)
	if _, err := io.Copy(io.Discard, &contextReader{ctx: ctx, reader: verified}); err != nil {
		return err
	}
	return verified.Verify()
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

// archiveRoot reads only the OCI layout's small index file. ORAS owns all
// content access and graph transfer after this selection.
func archiveRoot(path string) (v1.Descriptor, error) {
	file, err := os.Open(path)
	if err != nil {
		return v1.Descriptor{}, err
	}
	defer func() { _ = file.Close() }()
	reader := tar.NewReader(file)
	for {
		header, err := reader.Next()
		if err != nil {
			return v1.Descriptor{}, fmt.Errorf("OCI archive has no index.json: %w", err)
		}
		if header.Name != "index.json" && header.Name != "./index.json" {
			continue
		}
		if header.Size < 0 || header.Size > maxMetadataBytes {
			return v1.Descriptor{}, fmt.Errorf("OCI index is too large")
		}
		var index v1.Index
		if err := json.NewDecoder(io.LimitReader(reader, header.Size)).Decode(&index); err != nil {
			return v1.Descriptor{}, fmt.Errorf("decode OCI index: %w", err)
		}
		if index.SchemaVersion != 2 || len(index.Manifests) != 1 {
			return v1.Descriptor{}, fmt.Errorf("OCI archive must contain exactly one root manifest")
		}
		root := index.Manifests[0]
		if root.MediaType != v1.MediaTypeImageManifest && root.MediaType != v1.MediaTypeImageIndex && root.MediaType != dockerManifestType && root.MediaType != dockerIndexType {
			return v1.Descriptor{}, fmt.Errorf("unsupported OCI archive root media type %q", root.MediaType)
		}
		if err := checkDescriptor(root); err != nil {
			return v1.Descriptor{}, err
		}
		return root, nil
	}
}
