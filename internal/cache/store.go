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

	"coopr/internal/imageconfig"
	"coopr/internal/oci"
	"coopr/internal/stateidentity"
	"github.com/gofrs/flock"
	digest "github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content"
	orasoci "oras.land/oras-go/v2/content/oci"
	"oras.land/oras-go/v2/errdef"
)

const (
	RecordVersion     = "coopr.strio.dev/cache-record/v1"
	ArtifactType      = "application/vnd.coopr.strio.dev.cache.v1+json"
	ConfigMediaType   = "application/vnd.coopr.strio.dev.cache.config.v1+json"
	SnapshotMediaType = oci.ComponentPackageType
	maxRecordBytes    = 8 << 20
)

var ErrMiss = errors.New("portable cache miss")

// Record contains the logical output. Config is the full pre-return image
// config, but restoration must rebind rootfs/history to the current caller.
type Record struct {
	Version   string                 `json:"version"`
	CreatedAt time.Time              `json:"created_at"`
	Key       Key                    `json:"key"`
	Output    stateidentity.Identity `json:"output"`
	Config    json.RawMessage        `json:"config"`
	ChangedFS bool                   `json:"changedFs"`
	Snapshot  *oci.Package           `json:"snapshot,omitempty"`
	// Descriptor is assigned only after Lookup verifies the artifact.
	Descriptor v1.Descriptor `json:"-"`
}

// Store returns a verified snapshot tar in the caller-owned staging directory.
// The caller removes the returned path when done; Put never owns tarPath.
type Store interface {
	Lookup(context.Context, Key) (*Record, string, error)
	Put(context.Context, Key, Record, string) (v1.Descriptor, error)
}

type OCIStore struct {
	target     oras.Target
	stagingDir string
	localRoot  string
	ttl        *time.Duration
}

// RecordFresh applies the optional portable-cache age limit to validated metadata.
func RecordFresh(created time.Time, ttl *time.Duration, now time.Time) bool {
	if ttl == nil {
		return true
	}
	if *ttl <= 0 || created.IsZero() {
		return false
	}
	return !created.Before(now.Add(-*ttl))
}

func NewLocalStore(ctx context.Context, root, stagingDir string, ttl *time.Duration) (*OCIStore, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if root == "" {
		return nil, errors.New("empty cache layout path")
	}
	if err := checkStaging(stagingDir); err != nil {
		return nil, err
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}
	lock := flock.New(filepath.Join(root, ".coopr-cache.lock"))
	locked, err := lock.TryLockContext(ctx, 25*time.Millisecond)
	if err != nil {
		return nil, err
	}
	if !locked {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return nil, errors.New("cache layout lock not acquired")
	}
	defer func() { _ = lock.Unlock() }()
	target, err := orasoci.NewWithContext(ctx, root)
	if err != nil {
		return nil, err
	}
	return &OCIStore{target: target, stagingDir: stagingDir, localRoot: root, ttl: ttl}, nil
}

func NewRegistryStore(resolver *oci.Resolver, repository, stagingDir string, ttl *time.Duration) (*OCIStore, error) {
	if err := checkStaging(stagingDir); err != nil {
		return nil, err
	}
	if resolver == nil {
		return nil, errors.New("nil OCI resolver")
	}
	target, err := resolver.CacheRepository(repository)
	if err != nil {
		return nil, err
	}
	return &OCIStore{target: target, stagingDir: stagingDir, ttl: ttl}, nil
}

func checkStaging(dir string) error {
	if dir == "" {
		return errors.New("empty caller-owned cache staging directory")
	}
	info, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("cache staging path is not a directory")
	}
	return nil
}

func (r Record) validate(key Key) error {
	if r.Version != RecordVersion {
		return fmt.Errorf("unsupported cache record %q", r.Version)
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
		return errors.New("cache record key differs from request")
	}
	for _, d := range []digest.Digest{r.Output.Filesystem, r.Output.Configuration, r.Output.State} {
		if err := validSHA(d); err != nil {
			return err
		}
	}
	if len(r.Config) == 0 || len(r.Config) > maxRecordBytes {
		return errors.New("invalid logical output config size")
	}
	if _, err := imageconfig.Parse(r.Config); err != nil {
		return fmt.Errorf("logical output config: %w", err)
	}
	var config v1.Image
	if err := json.Unmarshal(r.Config, &config); err != nil {
		return err
	}
	if !samePlatform(config.Platform, key.Platform) {
		return errors.New("logical output platform differs from key")
	}
	if r.ChangedFS != (r.Snapshot != nil) {
		return errors.New("changed filesystem requires exactly one snapshot")
	}
	if !r.ChangedFS && r.Output.Filesystem != key.Input.Filesystem {
		return errors.New("unchanged filesystem has different output identity")
	}
	if r.Snapshot != nil {
		if r.Snapshot.Stage == "" {
			return errors.New("snapshot stage is required")
		}
		if r.Snapshot.Descriptor.MediaType != SnapshotMediaType {
			return errors.New("invalid snapshot media type")
		}
		if err := validDescriptor(r.Snapshot.Descriptor); err != nil {
			return err
		}
		if len(r.Snapshot.Config) > maxRecordBytes {
			return errors.New("snapshot config too large")
		}
		var image v1.Image
		if err := json.Unmarshal(r.Snapshot.Config, &image); err != nil {
			return fmt.Errorf("snapshot config: %w", err)
		}
		if !samePlatform(image.Platform, key.Platform) || image.RootFS.Type != "layers" || len(image.RootFS.DiffIDs) != 1 || image.RootFS.DiffIDs[0] != r.Snapshot.Descriptor.Digest {
			return errors.New("snapshot config does not describe tar/platform")
		}
	}
	return nil
}

func cacheTag(key digest.Digest) string { return "cache-" + key.Encoded() }

// operationTarget reloads the local index while holding the layout's
// cross-process lock. ORAS keeps its index in memory and writes the complete
// index on Tag, so a persistent target would lose another process's tags.
func (s *OCIStore) operationTarget(ctx context.Context) (oras.Target, func(), error) {
	if s.localRoot == "" {
		return s.target, func() {}, nil
	}
	lock := flock.New(filepath.Join(s.localRoot, ".coopr-cache.lock"))
	locked, err := lock.TryLockContext(ctx, 25*time.Millisecond)
	if err != nil {
		return nil, nil, err
	}
	if !locked {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		return nil, nil, errors.New("cache layout lock not acquired")
	}
	target, err := orasoci.NewWithContext(ctx, s.localRoot)
	if err != nil {
		_ = lock.Unlock()
		return nil, nil, err
	}
	return target, func() { _ = lock.Unlock() }, nil
}

func (s *OCIStore) Put(ctx context.Context, key Key, record Record, tarPath string) (v1.Descriptor, error) {
	if s == nil || s.target == nil {
		return v1.Descriptor{}, errors.New("nil cache store")
	}
	if record.Version == "" {
		record.Version = RecordVersion
	}
	if err := record.validate(key); err != nil {
		return v1.Descriptor{}, err
	}
	if record.ChangedFS {
		if tarPath == "" {
			return v1.Descriptor{}, errors.New("missing snapshot tar")
		}
		if err := verifyFile(ctx, tarPath, record.Snapshot.Descriptor); err != nil {
			return v1.Descriptor{}, err
		}
	} else if tarPath != "" {
		return v1.Descriptor{}, errors.New("unchanged filesystem cannot carry a snapshot tar")
	}
	data, err := json.Marshal(record)
	if err != nil {
		return v1.Descriptor{}, err
	}
	if len(data) > maxRecordBytes {
		return v1.Descriptor{}, errors.New("cache record too large")
	}
	target, release, err := s.operationTarget(ctx)
	if err != nil {
		return v1.Descriptor{}, err
	}
	defer release()
	config := oci.Descriptor(ConfigMediaType, data)
	var layers []v1.Descriptor
	if record.Snapshot != nil {
		layers = []v1.Descriptor{record.Snapshot.Descriptor}
	}
	manifestData, err := json.Marshal(oci.VersionedManifest(config, layers, ArtifactType))
	if err != nil {
		return v1.Descriptor{}, err
	}
	root := oci.Descriptor(v1.MediaTypeImageManifest, manifestData)
	if record.Snapshot != nil {
		file, err := os.Open(tarPath)
		if err != nil {
			return v1.Descriptor{}, err
		}
		pushErr := target.Push(ctx, record.Snapshot.Descriptor, file)
		_ = file.Close()
		if pushErr != nil && !errors.Is(pushErr, errdef.ErrAlreadyExists) {
			return v1.Descriptor{}, pushErr
		}
	}
	if err := target.Push(ctx, config, bytes.NewReader(data)); err != nil && !errors.Is(err, errdef.ErrAlreadyExists) {
		return v1.Descriptor{}, err
	}
	if err := target.Push(ctx, root, bytes.NewReader(manifestData)); err != nil && !errors.Is(err, errdef.ErrAlreadyExists) {
		return v1.Descriptor{}, err
	}
	if err := ctx.Err(); err != nil {
		return v1.Descriptor{}, err
	}
	keyDigest, _ := key.Digest()
	if err := target.Tag(ctx, root, cacheTag(keyDigest)); err != nil {
		return v1.Descriptor{}, err
	}
	return root, nil
}

func (s *OCIStore) Lookup(ctx context.Context, key Key) (*Record, string, error) {
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
	root, err := target.Resolve(ctx, cacheTag(keyDigest))
	if errors.Is(err, errdef.ErrNotFound) {
		return nil, "", ErrMiss
	}
	if err != nil {
		return nil, "", err
	}
	// ORAS carries the index tag as descriptor metadata on Resolve. The
	// artifact descriptor itself is the content-addressed triple below.
	if len(root.Annotations) == 1 && root.Annotations[v1.AnnotationRefName] == cacheTag(keyDigest) {
		root.Annotations = nil
	}
	if root.MediaType != v1.MediaTypeImageManifest {
		return nil, "", errors.New("invalid cache manifest media type")
	}
	manifestData, err := fetchVerified(ctx, target, root, maxRecordBytes)
	if err != nil {
		return nil, "", fmt.Errorf("cache manifest: %w", err)
	}
	var manifest v1.Manifest
	if err := strictJSON(manifestData, &manifest); err != nil {
		return nil, "", err
	}
	if manifest.SchemaVersion != 2 || manifest.MediaType != "" && manifest.MediaType != v1.MediaTypeImageManifest || manifest.ArtifactType != ArtifactType || manifest.Config.MediaType != ConfigMediaType || len(manifest.Layers) > 1 || manifest.Subject != nil {
		return nil, "", errors.New("invalid cache artifact manifest")
	}
	if err := validDescriptor(manifest.Config); err != nil {
		return nil, "", err
	}
	for _, layer := range manifest.Layers {
		if err := validDescriptor(layer); err != nil {
			return nil, "", err
		}
	}
	configData, err := fetchVerified(ctx, target, manifest.Config, maxRecordBytes)
	if err != nil {
		return nil, "", fmt.Errorf("cache config: %w", err)
	}
	var record Record
	if err := strictJSON(configData, &record); err != nil {
		return nil, "", err
	}
	if err := record.validate(key); err != nil {
		return nil, "", err
	}
	if len(manifest.Layers) == 0 && record.Snapshot != nil || len(manifest.Layers) == 1 && (record.Snapshot == nil || !sameDescriptor(manifest.Layers[0], record.Snapshot.Descriptor)) {
		return nil, "", errors.New("cache snapshot differs from manifest")
	}
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	if !RecordFresh(record.CreatedAt, s.ttl, time.Now()) {
		return nil, "", ErrMiss
	}
	if record.Snapshot == nil {
		record.Descriptor = root
		return &record, "", nil
	}
	if err := checkStaging(s.stagingDir); err != nil {
		return nil, "", err
	}
	file, err := os.CreateTemp(s.stagingDir, "coopr-cache-*.tar")
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
	stream, err := target.Fetch(ctx, record.Snapshot.Descriptor)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = stream.Close() }()
	verified := content.NewVerifyReader(stream, record.Snapshot.Descriptor)
	_, err = io.Copy(file, &contextReader{ctx: ctx, reader: verified})
	if err != nil {
		return nil, "", err
	}
	if err = verified.Verify(); err != nil {
		return nil, "", fmt.Errorf("cache snapshot: %w", err)
	}
	if err = file.Sync(); err != nil {
		return nil, "", err
	}
	if err = file.Close(); err != nil {
		return nil, "", err
	}
	keep = true
	record.Descriptor = root
	return &record, path, nil
}

func fetchVerified(ctx context.Context, target oras.Target, desc v1.Descriptor, limit int64) ([]byte, error) {
	if err := validDescriptor(desc); err != nil {
		return nil, err
	}
	if desc.Size > limit {
		return nil, errors.New("cache metadata exceeds size limit")
	}
	stream, err := target.Fetch(ctx, desc)
	if err != nil {
		return nil, err
	}
	defer func() { _ = stream.Close() }()
	verified := content.NewVerifyReader(stream, desc)
	data, err := io.ReadAll(io.LimitReader(&contextReader{ctx: ctx, reader: verified}, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errors.New("cache metadata exceeds size limit")
	}
	if err := verified.Verify(); err != nil {
		return nil, err
	}
	return data, nil
}

func verifyFile(ctx context.Context, path string, desc v1.Descriptor) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() != desc.Size {
		return errors.New("snapshot tar size differs from descriptor")
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	verified := content.NewVerifyReader(file, desc)
	_, err = io.Copy(io.Discard, &contextReader{ctx: ctx, reader: verified})
	if err != nil {
		return err
	}
	return verified.Verify()
}

func sameDescriptor(a, b v1.Descriptor) bool {
	return a.MediaType == b.MediaType && a.Digest == b.Digest && a.Size == b.Size
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
func strictJSON(data []byte, out any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return err
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("trailing cache JSON")
	}
	return nil
}
