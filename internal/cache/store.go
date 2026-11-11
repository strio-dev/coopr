package cache

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/gofrs/flock"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale"
	"github.com/strio-dev/coopr/internal/oci"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/content/memory"
	orasoci "oras.land/oras-go/v2/content/oci"
	"oras.land/oras-go/v2/errdef"
)

var ErrMiss = errors.New("portable cache miss")

type OCIStore struct {
	target     oras.Target
	stagingDir string
	localRoot  string
	ttl        *time.Duration
	resolver   *oci.Resolver
	repository string
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
	return &OCIStore{target: target, stagingDir: stagingDir, ttl: ttl, resolver: resolver, repository: repository}, nil
}

// cachePublicationSource adds only cache metadata to a caller-owned image
// layout. Bale reads the complete graph without copying its payloads to staging
// or modifying the source layout.
type cachePublicationSource struct {
	oras.ReadOnlyTarget
	metadata *cachePublicationMetadata
}

func newCachePublicationSource(source oras.ReadOnlyTarget) *cachePublicationSource {
	return &cachePublicationSource{ReadOnlyTarget: source, metadata: &cachePublicationMetadata{Store: memory.New()}}
}

func (s *cachePublicationSource) Fetch(ctx context.Context, descriptor v1.Descriptor) (io.ReadCloser, error) {
	exists, err := s.metadata.Exists(ctx, descriptor)
	if err != nil {
		return nil, err
	}
	if exists {
		return s.metadata.Fetch(ctx, descriptor)
	}
	return s.ReadOnlyTarget.Fetch(ctx, descriptor)
}

func (s *cachePublicationSource) Exists(ctx context.Context, descriptor v1.Descriptor) (bool, error) {
	exists, err := s.metadata.Exists(ctx, descriptor)
	if err != nil || exists {
		return exists, err
	}
	return s.ReadOnlyTarget.Exists(ctx, descriptor)
}

func (s *cachePublicationSource) Resolve(ctx context.Context, reference string) (v1.Descriptor, error) {
	if err := ctx.Err(); err != nil {
		return v1.Descriptor{}, err
	}
	descriptor, err := s.metadata.Resolve(ctx, reference)
	if !errors.Is(err, errdef.ErrNotFound) {
		return descriptor, err
	}
	return s.ReadOnlyTarget.Resolve(ctx, reference)
}

// Expose the layout's real CAS descriptor lookup so Bale plans payloads without
// buffering them. Source graphs were streamed and verified before this overlay
// was created; Bale also verifies bytes transferred and the published closure.
func (s *cachePublicationSource) Blobs() bale.RegistryBlobStore {
	return cachePublicationBlobs{s}
}

type cachePublicationBlobs struct{ *cachePublicationSource }

func (cachePublicationBlobs) Push(context.Context, v1.Descriptor, io.Reader) error {
	return errdef.ErrUnsupported
}

type cachePublicationMetadata struct{ *memory.Store }

func (s *cachePublicationMetadata) Push(ctx context.Context, descriptor v1.Descriptor, reader io.Reader) error {
	if err := s.Store.Push(ctx, descriptor, reader); err != nil && !errors.Is(err, errdef.ErrAlreadyExists) {
		return err
	}
	return s.Tag(ctx, descriptor, descriptor.Digest.String())
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

func fetchVerified(ctx context.Context, target content.Fetcher, desc v1.Descriptor, limit int64) (_ []byte, retErr error) {
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
	defer func() { retErr = errors.Join(retErr, stream.Close()) }()
	return content.ReadAll(&contextReader{ctx: ctx, reader: stream}, desc)
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
