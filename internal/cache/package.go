package cache

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"coopr/internal/oci"
	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/errdef"
)

const (
	PackageKeyVersion      = "coopr.strio.dev/package-cache-key/v1"
	PackageRecordVersion   = "coopr.strio.dev/package-cache-record/v1"
	PackageArtifactType    = "application/vnd.coopr.strio.dev.package-cache.v1+json"
	PackageConfigMediaType = "application/vnd.coopr.strio.dev.package-cache.config.v1+json"
	maxPackageRecordBytes  = 32 << 20
)

type PackageStore interface {
	LookupPackage(context.Context, PackageKey) (*PackageRecord, string, error)
	PutPackage(context.Context, PackageKey, PackageRecord, string) (v1.Descriptor, error)
}

// PackageKey identifies one package output from a component publication
// graph. Plan is the digest of the selected output's ordered stage closure;
// Bases records the immutable manifests selected for all external images in
// that closure. Context is present only when the closure reads the filtered
// build context.
type PackageKey struct {
	Plan     digest.Digest            `json:"plan"`
	Output   string                   `json:"output"`
	Bases    map[string]v1.Descriptor `json:"bases,omitempty"`
	Context  digest.Digest            `json:"context,omitempty"`
	Platform v1.Platform              `json:"platform"`
	Executor string                   `json:"executor"`
	Frontend string                   `json:"frontend"`
	Lowering string                   `json:"lowering"`
}

func (k PackageKey) Validate() error {
	if err := validSHA(k.Plan); err != nil {
		return fmt.Errorf("package plan: %w", err)
	}
	if err := validName(k.Output); err != nil {
		return fmt.Errorf("package output: %w", err)
	}
	for name, descriptor := range k.Bases {
		if err := validName(name); err != nil {
			return fmt.Errorf("package base: %w", err)
		}
		if err := validDescriptor(descriptor); err != nil {
			return fmt.Errorf("package base %q: %w", name, err)
		}
	}
	if k.Context != "" {
		if err := validSHA(k.Context); err != nil {
			return fmt.Errorf("package context: %w", err)
		}
	}
	if err := validPlatform(k.Platform); err != nil {
		return err
	}
	for _, value := range []string{k.Executor, k.Frontend, k.Lowering} {
		if err := validName(value); err != nil {
			return fmt.Errorf("invalid package-cache semantic version: %w", err)
		}
	}
	return nil
}

func (k PackageKey) Digest() (digest.Digest, error) {
	if err := k.Validate(); err != nil {
		return "", err
	}
	k.Platform = normalizePlatform(k.Platform)
	data, err := json.Marshal(struct {
		Version string     `json:"version"`
		Key     PackageKey `json:"key"`
	}{Version: PackageKeyVersion, Key: k})
	if err != nil {
		return "", err
	}
	return digest.FromBytes(data), nil
}

// PackageRecord is deliberately separate from an invocation-result Record:
// the portable boundary is the completed component package tar and its
// authoritative one-layer image configuration.
type PackageRecord struct {
	CreatedAt  time.Time       `json:"created_at"`
	Version    string          `json:"version"`
	Key        PackageKey      `json:"key"`
	Stage      string          `json:"stage"`
	Descriptor v1.Descriptor   `json:"descriptor"`
	Config     json.RawMessage `json:"config"`
	Artifact   v1.Descriptor   `json:"-"`
}

func (r PackageRecord) validate(key PackageKey) error {
	if r.Version != PackageRecordVersion {
		return fmt.Errorf("unsupported package cache record %q", r.Version)
	}
	want, err := key.Digest()
	if err != nil {
		return err
	}
	got, err := r.Key.Digest()
	if err != nil {
		return err
	}
	if want != got {
		return errors.New("package cache record key differs from request")
	}
	if r.Stage != key.Output {
		return errors.New("package cache stage differs from output")
	}
	if r.Descriptor.MediaType != oci.ComponentPackageType {
		return errors.New("invalid package cache snapshot media type")
	}
	if err := validDescriptor(r.Descriptor); err != nil {
		return err
	}
	if len(r.Config) == 0 || len(r.Config) > maxPackageRecordBytes {
		return errors.New("invalid package cache config size")
	}
	var image v1.Image
	if err := json.Unmarshal(r.Config, &image); err != nil {
		return fmt.Errorf("package cache image config: %w", err)
	}
	if !samePlatform(image.Platform, key.Platform) || image.RootFS.Type != "layers" || len(image.RootFS.DiffIDs) != 1 || image.RootFS.DiffIDs[0] != r.Descriptor.Digest {
		return errors.New("package cache config does not describe tar/platform")
	}
	return nil
}

func packageCacheTag(key digest.Digest) string { return "package-" + key.Encoded() }

func (s *OCIStore) PutPackage(ctx context.Context, key PackageKey, record PackageRecord, tarPath string) (v1.Descriptor, error) {
	if s == nil || s.target == nil {
		return v1.Descriptor{}, errors.New("nil cache store")
	}
	if record.Version == "" {
		record.Version = PackageRecordVersion
	}
	if err := record.validate(key); err != nil {
		return v1.Descriptor{}, err
	}
	if tarPath == "" {
		return v1.Descriptor{}, errors.New("missing package snapshot tar")
	}
	if err := verifyFile(ctx, tarPath, record.Descriptor); err != nil {
		return v1.Descriptor{}, err
	}
	data, err := json.Marshal(record)
	if err != nil {
		return v1.Descriptor{}, err
	}
	if len(data) > maxPackageRecordBytes {
		return v1.Descriptor{}, errors.New("package cache record too large")
	}
	target, release, err := s.operationTarget(ctx)
	if err != nil {
		return v1.Descriptor{}, err
	}
	defer release()
	config := oci.Descriptor(PackageConfigMediaType, data)
	manifestData, err := json.Marshal(oci.VersionedManifest(config, []v1.Descriptor{record.Descriptor}, PackageArtifactType))
	if err != nil {
		return v1.Descriptor{}, err
	}
	root := oci.Descriptor(v1.MediaTypeImageManifest, manifestData)
	file, err := os.Open(tarPath)
	if err != nil {
		return v1.Descriptor{}, err
	}
	pushErr := target.Push(ctx, record.Descriptor, file)
	_ = file.Close()
	if pushErr != nil && !errors.Is(pushErr, errdef.ErrAlreadyExists) {
		return v1.Descriptor{}, pushErr
	}
	if err := target.Push(ctx, config, bytes.NewReader(data)); err != nil && !errors.Is(err, errdef.ErrAlreadyExists) {
		return v1.Descriptor{}, err
	}
	if err := target.Push(ctx, root, bytes.NewReader(manifestData)); err != nil && !errors.Is(err, errdef.ErrAlreadyExists) {
		return v1.Descriptor{}, err
	}
	keyDigest, _ := key.Digest()
	if err := target.Tag(ctx, root, packageCacheTag(keyDigest)); err != nil {
		return v1.Descriptor{}, err
	}
	return root, nil
}

func (s *OCIStore) LookupPackage(ctx context.Context, key PackageKey) (*PackageRecord, string, error) {
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
	root, err := target.Resolve(ctx, packageCacheTag(keyDigest))
	if err != nil {
		// Both local OCI and registry targets surface missing tags through the
		// same helper used by ordinary cache records.
		if errors.Is(err, errdef.ErrNotFound) {
			return nil, "", ErrMiss
		}
		return nil, "", err
	}
	if len(root.Annotations) == 1 && root.Annotations[v1.AnnotationRefName] == packageCacheTag(keyDigest) {
		root.Annotations = nil
	}
	if root.MediaType != v1.MediaTypeImageManifest {
		return nil, "", errors.New("invalid package cache manifest media type")
	}
	manifestData, err := fetchVerified(ctx, target, root, maxPackageRecordBytes)
	if err != nil {
		return nil, "", fmt.Errorf("package cache manifest: %w", err)
	}
	var manifest v1.Manifest
	if err := strictJSON(manifestData, &manifest); err != nil {
		return nil, "", err
	}
	if manifest.SchemaVersion != 2 || manifest.MediaType != "" && manifest.MediaType != v1.MediaTypeImageManifest || manifest.ArtifactType != PackageArtifactType || manifest.Config.MediaType != PackageConfigMediaType || len(manifest.Layers) != 1 || manifest.Subject != nil {
		return nil, "", errors.New("invalid package cache artifact manifest")
	}
	if err := validDescriptor(manifest.Config); err != nil {
		return nil, "", err
	}
	if err := validDescriptor(manifest.Layers[0]); err != nil {
		return nil, "", err
	}
	configData, err := fetchVerified(ctx, target, manifest.Config, maxPackageRecordBytes)
	if err != nil {
		return nil, "", fmt.Errorf("package cache config: %w", err)
	}
	var record PackageRecord
	if err := strictJSON(configData, &record); err != nil {
		return nil, "", err
	}
	if err := record.validate(key); err != nil {
		return nil, "", err
	}
	if !sameDescriptor(manifest.Layers[0], record.Descriptor) {
		return nil, "", errors.New("package cache snapshot differs from manifest")
	}
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	if !RecordFresh(record.CreatedAt, s.ttl, time.Now()) {
		return nil, "", ErrMiss
	}
	file, err := os.CreateTemp(s.stagingDir, "coopr-package-cache-*.tar")
	if err != nil {
		return nil, "", err
	}
	path := file.Name()
	keep := false
	defer func() {
		_ = file.Close()
		if !keep {
			_ = os.Remove(path)
		}
	}()
	stream, err := target.Fetch(ctx, record.Descriptor)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = stream.Close() }()
	verified := content.NewVerifyReader(stream, record.Descriptor)
	if _, err := file.ReadFrom(&contextReader{ctx: ctx, reader: verified}); err != nil {
		return nil, "", err
	}
	if err := verified.Verify(); err != nil {
		return nil, "", fmt.Errorf("package cache snapshot: %w", err)
	}
	if err := file.Sync(); err != nil {
		return nil, "", err
	}
	if err := file.Close(); err != nil {
		return nil, "", err
	}
	keep = true
	record.Artifact = root
	return &record, path, nil
}
