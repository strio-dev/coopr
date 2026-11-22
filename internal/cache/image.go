package cache

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/strio-dev/coopr/internal/oci"

	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content"
	orasoci "oras.land/oras-go/v2/content/oci"
	"oras.land/oras-go/v2/errdef"
)

const (
	ImageKeyVersion      = "coopr.strio.dev/instruction-image-cache-key/v2"
	ImageRecordVersion   = "coopr.strio.dev/instruction-image-cache-record/v2"
	ImageArtifactType    = "application/vnd.coopr.strio.dev.instruction-image-cache.v2+json"
	ImageConfigMediaType = "application/vnd.coopr.strio.dev.instruction-image-cache.config.v2+json"
	maxImageRecordBytes  = 8 << 20
	instructionTagPrefix = "instruction-"
)

// ImageKey identifies one exact instruction checkpoint image. Parent is the
// ordered OCI rootfs chain rather than image/config creation metadata, so the
// key is stable across machines and equivalent checkpoint configurations.
type ImageKey struct {
	Instruction digest.Digest `json:"instruction"`
	Parent      digest.Digest `json:"parent"`
	Platform    v1.Platform   `json:"platform"`
	Executor    string        `json:"executor"`
	Format      string        `json:"format"`
}

func (k ImageKey) Validate() error {
	if err := validSHA(k.Instruction); err != nil {
		return fmt.Errorf("instruction: %w", err)
	}
	if err := validSHA(k.Parent); err != nil {
		return fmt.Errorf("parent: %w", err)
	}
	if err := validPlatform(k.Platform); err != nil {
		return err
	}
	if err := validName(k.Executor); err != nil {
		return fmt.Errorf("executor: %w", err)
	}
	if k.Format != "oci" && k.Format != "docker" {
		return fmt.Errorf("invalid image format %q", k.Format)
	}
	return nil
}

func (k ImageKey) Digest() (digest.Digest, error) {
	if err := k.Validate(); err != nil {
		return "", err
	}
	k.Platform = normalizePlatform(k.Platform)
	data, err := json.Marshal(struct {
		Version string   `json:"version"`
		Key     ImageKey `json:"key"`
	}{ImageKeyVersion, k})
	if err != nil {
		return "", err
	}
	return digest.FromBytes(data), nil
}

type ImageRecord struct {
	CommandSet    *bool           `json:"command_set,omitempty"`
	RetainsCaller *bool           `json:"retains_caller,omitempty"`
	Version       string          `json:"version"`
	CreatedAt     time.Time       `json:"created_at"`
	Key           ImageKey        `json:"key"`
	Image         v1.Descriptor   `json:"image"`
	RootMetadata  json.RawMessage `json:"root_metadata,omitempty"`
	LogicalConfig json.RawMessage `json:"logical_config,omitempty"`
	BaseImage     *v1.Descriptor  `json:"base_image,omitempty"`
}

func (r ImageRecord) validate(key ImageKey) error {
	if r.Version != ImageRecordVersion {
		return fmt.Errorf("unsupported instruction image record %q", r.Version)
	}
	want, err := key.Digest()
	if err != nil {
		return err
	}
	got, err := r.Key.Digest()
	if err != nil || got != want {
		return errors.New("instruction image cache record key differs from request")
	}
	if r.Image.MediaType != v1.MediaTypeImageManifest && r.Image.MediaType != "application/vnd.docker.distribution.manifest.v2+json" {
		return fmt.Errorf("unsupported cached image manifest type %q", r.Image.MediaType)
	}
	if len(r.LogicalConfig) != 0 && !json.Valid(r.LogicalConfig) {
		return errors.New("invalid cached logical image configuration")
	}
	if r.BaseImage != nil {
		if err := validDescriptor(*r.BaseImage); err != nil {
			return err
		}
		if r.BaseImage.MediaType != v1.MediaTypeImageManifest && r.BaseImage.MediaType != "application/vnd.docker.distribution.manifest.v2+json" {
			return errors.New("invalid cached base image manifest type")
		}
	}
	return validDescriptor(r.Image)
}

func instructionImageTag(key digest.Digest) string { return instructionTagPrefix + key.Encoded() }

// PutImage stores an exact OCI image graph and tags a small cache artifact
// only after every graph blob is available.
func (s *OCIStore) PutImage(ctx context.Context, key ImageKey, record ImageRecord, layoutPath string) (v1.Descriptor, error) {
	if s == nil || s.target == nil {
		return v1.Descriptor{}, errors.New("nil cache store")
	}
	if record.Version == "" {
		record.Version = ImageRecordVersion
	}
	if err := record.validate(key); err != nil {
		return v1.Descriptor{}, err
	}
	source, err := orasoci.New(layoutPath)
	if err != nil {
		return v1.Descriptor{}, err
	}
	if err := verifyImageGraph(ctx, source, record.Image); err != nil {
		return v1.Descriptor{}, fmt.Errorf("verify instruction image graph: %w", err)
	}
	if record.BaseImage != nil {
		if err := verifyImageGraph(ctx, source, *record.BaseImage); err != nil {
			return v1.Descriptor{}, fmt.Errorf("verify cached base image graph: %w", err)
		}
	}
	target, release, err := s.operationTarget(ctx)
	if err != nil {
		return v1.Descriptor{}, err
	}
	defer release()
	graphs := []v1.Descriptor{record.Image}
	if record.BaseImage != nil {
		graphs = append(graphs, *record.BaseImage)
	}
	if err := copyGraphImages(ctx, source, target, graphs, false, s.localRoot != ""); err != nil {
		return v1.Descriptor{}, fmt.Errorf("store instruction image graph: %w", err)
	}
	data, err := json.Marshal(record)
	if err != nil || len(data) > maxImageRecordBytes {
		return v1.Descriptor{}, errors.Join(err, errors.New("instruction image cache record too large"))
	}
	config := oci.Descriptor(ImageConfigMediaType, data)
	manifestData, err := json.Marshal(oci.VersionedManifest(config, []v1.Descriptor{}, ImageArtifactType))
	if err != nil {
		return v1.Descriptor{}, err
	}
	if len(manifestData) > maxImageRecordBytes {
		return v1.Descriptor{}, errors.New("instruction image cache metadata manifest too large")
	}
	metadata := oci.Descriptor(v1.MediaTypeImageManifest, manifestData)
	manifests := []v1.Descriptor{metadata, record.Image}
	if record.BaseImage != nil {
		manifests = append(manifests, *record.BaseImage)
	}
	indexData, err := json.Marshal(oci.VersionedIndex(manifests, ImageArtifactType))
	if err != nil {
		return v1.Descriptor{}, err
	}
	if len(indexData) > maxImageRecordBytes {
		return v1.Descriptor{}, errors.New("instruction image cache index too large")
	}
	root := oci.Descriptor(v1.MediaTypeImageIndex, indexData)
	if err := target.Push(ctx, config, bytes.NewReader(data)); err != nil && !errors.Is(err, errdef.ErrAlreadyExists) {
		return v1.Descriptor{}, err
	}
	if err := target.Push(ctx, metadata, bytes.NewReader(manifestData)); err != nil && !errors.Is(err, errdef.ErrAlreadyExists) {
		return v1.Descriptor{}, err
	}
	if s.localRoot == "" {
		keyDigest, _ := key.Digest()
		return oras.TagBytes(ctx, target, v1.MediaTypeImageIndex, indexData, instructionImageTag(keyDigest))
	}
	if err := target.Push(ctx, root, bytes.NewReader(indexData)); err != nil && !errors.Is(err, errdef.ErrAlreadyExists) {
		return v1.Descriptor{}, err
	}
	keyDigest, _ := key.Digest()
	if err := target.Tag(ctx, root, instructionImageTag(keyDigest)); err != nil {
		return v1.Descriptor{}, err
	}
	return root, nil
}

// LookupImage verifies the cache artifact and complete selected image graph,
// then materializes a caller-owned OCI layout containing the exact manifest.
func (s *OCIStore) LookupImage(ctx context.Context, key ImageKey) (*ImageRecord, string, error) {
	if s == nil || s.target == nil {
		return nil, "", errors.New("nil cache store")
	}
	keyDigest, err := key.Digest()
	if err != nil {
		return nil, "", err
	}
	target, release, err := s.operationTarget(ctx)
	if err != nil {
		return nil, "", err
	}
	defer release()
	root, err := target.Resolve(ctx, instructionImageTag(keyDigest))
	if errors.Is(err, errdef.ErrNotFound) {
		return nil, "", ErrMiss
	}
	if err != nil {
		return nil, "", err
	}
	if len(root.Annotations) == 1 && root.Annotations[v1.AnnotationRefName] == instructionImageTag(keyDigest) {
		root.Annotations = nil
	}
	if root.MediaType != v1.MediaTypeImageIndex {
		return nil, "", errors.New("invalid instruction image cache index media type")
	}
	if err := validDescriptor(root); err != nil {
		return nil, "", err
	}
	indexData, err := fetchVerified(ctx, target, root, maxImageRecordBytes)
	if err != nil {
		return nil, "", err
	}
	var index v1.Index
	if err := strictJSON(indexData, &index); err != nil {
		return nil, "", err
	}
	if index.SchemaVersion != 2 || index.MediaType != "" && index.MediaType != v1.MediaTypeImageIndex || index.ArtifactType != ImageArtifactType || (len(index.Manifests) != 2 && len(index.Manifests) != 3) || index.Subject != nil {
		return nil, "", errors.New("invalid instruction image cache index")
	}
	for _, descriptor := range index.Manifests {
		if err := validDescriptor(descriptor); err != nil {
			return nil, "", err
		}
	}
	if index.Manifests[0].MediaType != v1.MediaTypeImageManifest {
		return nil, "", errors.New("invalid instruction image cache metadata manifest type")
	}
	manifestData, err := fetchVerified(ctx, target, index.Manifests[0], maxImageRecordBytes)
	if err != nil {
		return nil, "", err
	}
	var artifact v1.Manifest
	if err := strictJSON(manifestData, &artifact); err != nil {
		return nil, "", err
	}
	if artifact.SchemaVersion != 2 || artifact.MediaType != "" && artifact.MediaType != v1.MediaTypeImageManifest || artifact.ArtifactType != ImageArtifactType || artifact.Config.MediaType != ImageConfigMediaType || artifact.Layers == nil || len(artifact.Layers) != 0 || artifact.Subject != nil {
		return nil, "", errors.New("invalid instruction image cache artifact")
	}
	if err := validDescriptor(artifact.Config); err != nil {
		return nil, "", err
	}
	data, err := fetchVerified(ctx, target, artifact.Config, maxImageRecordBytes)
	if err != nil {
		return nil, "", err
	}
	var record ImageRecord
	if err := strictJSON(data, &record); err != nil {
		return nil, "", err
	}
	if err := record.validate(key); err != nil {
		return nil, "", err
	}
	if !content.Equal(index.Manifests[1], record.Image) {
		return nil, "", errors.New("instruction image differs from cache artifact")
	}
	if (record.BaseImage == nil) != (len(index.Manifests) == 2) {
		return nil, "", errors.New("cached base image differs from artifact")
	}
	if record.BaseImage != nil {
		if !content.Equal(index.Manifests[2], *record.BaseImage) {
			return nil, "", errors.New("cached base image differs from artifact")
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	if !RecordFresh(record.CreatedAt, s.ttl, time.Now()) {
		return nil, "", ErrMiss
	}
	dir, err := os.MkdirTemp(s.stagingDir, "coopr-instruction-image-*")
	if err != nil {
		return nil, "", err
	}
	keep := false
	defer func() {
		if !keep {
			_ = os.RemoveAll(dir)
		}
	}()
	destination, err := orasoci.New(filepath.Clean(dir))
	if err != nil {
		return nil, "", err
	}
	graphs := []v1.Descriptor{record.Image}
	if record.BaseImage != nil {
		graphs = append(graphs, *record.BaseImage)
	}
	if err := materializeGraphImages(ctx, target, graphs, destination, nil); err != nil {
		return nil, "", err
	}
	keep = true
	return &record, dir, nil
}

func imageGraphDescriptors(ctx context.Context, target oras.Target, root v1.Descriptor) ([]v1.Descriptor, error) {
	if err := validDescriptor(root); err != nil {
		return nil, err
	}
	manifestData, err := fetchVerified(ctx, target, root, maxImageRecordBytes)
	if err != nil {
		return nil, err
	}
	var manifest v1.Manifest
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		return nil, err
	}
	if manifest.SchemaVersion != 2 || manifest.MediaType != "" && manifest.MediaType != root.MediaType || manifest.ArtifactType != "" || len(manifest.Layers) > 2048 || manifest.Subject != nil {
		return nil, errors.New("invalid cached image manifest")
	}
	if manifest.Config.MediaType != v1.MediaTypeImageConfig && manifest.Config.MediaType != "application/vnd.docker.container.image.v1+json" {
		return nil, fmt.Errorf("unsupported cached image config media type %q", manifest.Config.MediaType)
	}
	if err := validDescriptor(manifest.Config); err != nil {
		return nil, err
	}
	for _, descriptor := range manifest.Layers {
		// OCI layers may carry compression metadata such as zstd:chunked
		// offsets and checksums. Keep it in the verified manifest; annotations
		// do not relax the digest, size or local blob requirements.
		descriptor.Annotations = nil
		if err := validDescriptor(descriptor); err != nil {
			return nil, err
		}
	}
	return append([]v1.Descriptor{manifest.Config}, manifest.Layers...), nil
}

func verifyImageGraph(ctx context.Context, target oras.Target, root v1.Descriptor) error {
	descriptors, err := imageGraphDescriptors(ctx, target, root)
	if err != nil {
		return err
	}
	for _, descriptor := range descriptors {
		stream, err := target.Fetch(ctx, descriptor)
		if err != nil {
			return err
		}
		verified := content.NewVerifyReader(stream, descriptor)
		_, copyErr := io.Copy(io.Discard, verified)
		verifyErr := verified.Verify()
		closeErr := stream.Close()
		if err := errors.Join(copyErr, verifyErr, closeErr); err != nil {
			return fmt.Errorf("blob %s: %w", descriptor.Digest, err)
		}
	}
	return nil
}
