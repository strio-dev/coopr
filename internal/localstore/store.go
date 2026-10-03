// Package localstore manages OCI graphs in Coopr-owned local image layouts.
package localstore

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"coopr/internal/storeactivity"
	"github.com/distribution/reference"
	"github.com/gofrs/flock"
	"github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content"
	orasoci "oras.land/oras-go/v2/content/oci"
	"oras.land/oras-go/v2/errdef"
)

const lockFilename = ".coopr-store.lock"

type Entry struct {
	Reference  string        `json:"reference,omitempty"`
	Descriptor v1.Descriptor `json:"descriptor"`
}

type Usage struct {
	Roots int   `json:"roots"`
	Tags  int   `json:"tags"`
	Bytes int64 `json:"bytes"`
}

func DefaultImageDir() (string, error) { return defaultDir("images") }

func DefaultComponentDir() (string, error) { return defaultDir("components") }

// NormalizeImageTag applies the same local-name and latest defaults used when
// images are written to Coopr's local store. An implicit localhost prefix is
// removed again so a build tagged "base" is later found by FROM base.
func NormalizeImageTag(tag string) (string, error) {
	if tag == "" {
		return "", nil
	}
	if strings.TrimSpace(tag) != tag || strings.ContainsAny(tag, " \t\r\n") || strings.HasPrefix(tag, "-") || strings.Contains(tag, "://") {
		return "", fmt.Errorf("invalid local image tag %q", tag)
	}
	explicitLocalhost := strings.HasPrefix(tag, "localhost/")
	first := strings.SplitN(tag, "/", 2)[0]
	if !strings.Contains(tag, "/") || first != "localhost" && !strings.ContainsAny(first, ".:") {
		tag = "localhost/" + tag
	}
	named, err := reference.ParseNormalizedNamed(tag)
	if err != nil {
		return "", fmt.Errorf("invalid local image tag %q: %w", tag, err)
	}
	if _, ok := named.(reference.Digested); ok {
		return "", fmt.Errorf("local image tag %q must not include a digest", tag)
	}
	normalized := reference.TagNameOnly(named).String()
	if !explicitLocalhost && strings.HasPrefix(normalized, "localhost/") {
		normalized = strings.TrimPrefix(normalized, "localhost/")
	}
	return normalized, nil
}

func defaultDir(name string) (string, error) {
	if data := os.Getenv("XDG_DATA_HOME"); data != "" {
		if !filepath.IsAbs(data) {
			return "", errors.New("XDG_DATA_HOME must be an absolute path")
		}
		return filepath.Join(data, "coopr", name), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate home directory: %w", err)
	}
	return filepath.Join(home, ".local", "share", "coopr", name), nil
}

// Put copies a complete graph into a shared OCI layout. Content may be left
// unreferenced after cancellation, but tags are updated only after the complete
// graph has been copied while holding the cross-process lock.
func Put(ctx context.Context, rootDir string, source content.ReadOnlyStorage, root v1.Descriptor, tags ...string) error {
	if source == nil {
		return errors.New("nil OCI graph source")
	}
	dir, err := prepareRoot(rootDir)
	if err != nil {
		return err
	}
	lock := flock.New(filepath.Join(dir, lockFilename))
	locked, err := lock.TryLockContext(ctx, 25*time.Millisecond)
	if err != nil {
		return err
	}
	if !locked {
		if err := ctx.Err(); err != nil {
			return err
		}
		return errors.New("local OCI store lock not acquired")
	}
	defer func() { _ = lock.Unlock() }()
	target, err := orasoci.NewWithContext(ctx, dir)
	if err != nil {
		return err
	}
	if err := oras.CopyGraph(ctx, source, target, root, oras.CopyGraphOptions{}); err != nil {
		return fmt.Errorf("copy OCI graph: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := target.Tag(ctx, root, root.Digest.String()); err != nil {
		return fmt.Errorf("anchor local OCI graph: %w", err)
	}
	for _, tag := range tags {
		if tag == "" {
			continue
		}
		if err := target.Tag(ctx, root, tag); err != nil {
			return fmt.Errorf("tag local OCI graph %q: %w", tag, err)
		}
	}
	return nil
}

// TagExisting commits a graph whose blobs were written directly into this
// store. It verifies the rooted graph before publishing a name in the shared
// index.
func TagExisting(ctx context.Context, rootDir string, root v1.Descriptor, tags ...string) error {
	dir, err := prepareRoot(rootDir)
	if err != nil {
		return err
	}
	lock := flock.New(filepath.Join(dir, lockFilename))
	locked, err := lock.TryLockContext(ctx, 25*time.Millisecond)
	if err != nil {
		return err
	}
	if !locked {
		if err := ctx.Err(); err != nil {
			return err
		}
		return errors.New("local OCI store lock not acquired")
	}
	defer func() { _ = lock.Unlock() }()
	store, err := orasoci.NewWithContext(ctx, dir)
	if err != nil {
		return err
	}
	if err := verifyGraph(ctx, store, root); err != nil {
		return fmt.Errorf("verify local OCI graph: %w", err)
	}
	store.AutoSaveIndex = false
	if err := store.Tag(ctx, root, root.Digest.String()); err != nil {
		return fmt.Errorf("anchor local OCI graph: %w", err)
	}
	for _, tag := range tags {
		if tag == "" {
			continue
		}
		if err := store.Tag(ctx, root, tag); err != nil {
			return fmt.Errorf("tag local OCI graph %q: %w", tag, err)
		}
	}
	if err := store.SaveIndex(); err != nil {
		return fmt.Errorf("save local OCI index: %w", err)
	}
	return ctx.Err()
}

func verifyGraph(ctx context.Context, store content.ReadOnlyStorage, root v1.Descriptor) error {
	pending := []v1.Descriptor{root}
	seen := make(map[string]struct{})
	for len(pending) != 0 {
		desc := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		key := fmt.Sprintf("%s/%s/%d", desc.Digest, desc.MediaType, desc.Size)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		if desc.Digest.Validate() != nil || desc.Size < 0 || desc.MediaType == "" {
			return fmt.Errorf("invalid OCI descriptor %q", desc.Digest)
		}
		stream, err := store.Fetch(ctx, desc)
		if err != nil {
			return fmt.Errorf("fetch %s: %w", desc.Digest, err)
		}
		verified := content.NewVerifyReader(stream, desc)
		_, copyErr := io.Copy(io.Discard, &contextReader{ctx: ctx, r: verified})
		if copyErr != nil {
			_ = stream.Close()
			return fmt.Errorf("read %s: %w", desc.Digest, copyErr)
		}
		if err := verified.Verify(); err != nil {
			_ = stream.Close()
			return fmt.Errorf("verify %s: %w", desc.Digest, err)
		}
		if err := stream.Close(); err != nil {
			return fmt.Errorf("close %s: %w", desc.Digest, err)
		}
		next, err := content.Successors(ctx, store, desc)
		if err != nil {
			return fmt.Errorf("read successors of %s: %w", desc.Digest, err)
		}
		pending = append(pending, next...)
	}
	return nil
}

// Open opens a shared OCI layout after waiting for any writer to finish.
func Open(ctx context.Context, rootDir string) (*orasoci.Store, error) {
	dir, err := prepareRoot(rootDir)
	if err != nil {
		return nil, err
	}
	lock := flock.New(filepath.Join(dir, lockFilename))
	locked, err := lock.TryLockContext(ctx, 25*time.Millisecond)
	if err != nil {
		return nil, err
	}
	if !locked {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return nil, errors.New("local OCI store lock not acquired")
	}
	defer func() { _ = lock.Unlock() }()
	return orasoci.NewWithContext(ctx, dir)
}

// WithReadLock keeps the store index stable while a reader uses a positional
// OCI-layout reference. Writers take the same lock exclusively in Put.
func WithReadLock(ctx context.Context, rootDir string, read func() error) error {
	if read == nil {
		return errors.New("nil local OCI store reader")
	}
	dir, err := prepareRoot(rootDir)
	if err != nil {
		return err
	}
	lock := flock.New(filepath.Join(dir, lockFilename))
	locked, err := lock.TryRLockContext(ctx, 25*time.Millisecond)
	if err != nil {
		return err
	}
	if !locked {
		if err := ctx.Err(); err != nil {
			return err
		}
		return errors.New("local OCI store read lock not acquired")
	}
	defer func() { _ = lock.Unlock() }()
	return read()
}

// List returns the OCI layout roots and mutable tags from one stable index
// snapshot. Digest-only entries have an empty Reference.
func List(ctx context.Context, rootDir string) ([]Entry, error) {
	var entries []Entry
	err := WithReadLock(ctx, rootDir, func() error {
		data, err := os.ReadFile(filepath.Join(rootDir, v1.ImageIndexFile))
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		var index v1.Index
		if err := json.Unmarshal(data, &index); err != nil {
			return fmt.Errorf("decode local OCI index: %w", err)
		}
		for _, descriptor := range index.Manifests {
			entries = append(entries, Entry{Reference: descriptor.Annotations[v1.AnnotationRefName], Descriptor: descriptor})
		}
		return nil
	})
	slices.SortFunc(entries, func(a, b Entry) int {
		if a.Reference != b.Reference {
			return strings.Compare(a.Reference, b.Reference)
		}
		return strings.Compare(a.Descriptor.Digest.String(), b.Descriptor.Digest.String())
	})
	return entries, err
}

func Inspect(ctx context.Context, rootDir, reference string) (v1.Descriptor, bool, error) {
	var descriptor v1.Descriptor
	found := false
	err := WithReadLock(ctx, rootDir, func() error {
		store, err := orasoci.NewWithContext(ctx, rootDir)
		if err != nil {
			return err
		}
		descriptor, err = store.Resolve(ctx, reference)
		if err != nil {
			if errors.Is(err, errdef.ErrNotFound) {
				return nil
			}
			return err
		}
		found = true
		return nil
	})
	return descriptor, found, err
}

func RemoveTag(ctx context.Context, rootDir, reference string) (bool, error) {
	if reference == "" || digest.Digest(reference).Validate() == nil {
		return false, fmt.Errorf("invalid mutable OCI tag %q", reference)
	}
	dir, err := prepareRoot(rootDir)
	if err != nil {
		return false, err
	}
	lock := flock.New(filepath.Join(dir, lockFilename))
	locked, err := lock.TryLockContext(ctx, 25*time.Millisecond)
	if err != nil || !locked {
		if err != nil {
			return false, err
		}
		return false, errors.New("local OCI store lock not acquired")
	}
	defer func() { _ = lock.Unlock() }()
	store, err := orasoci.NewWithContext(ctx, dir)
	if err != nil {
		return false, err
	}
	if _, err := store.Resolve(ctx, reference); err != nil {
		if errors.Is(err, errdef.ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	if err := store.Untag(ctx, reference); err != nil {
		return false, err
	}
	return true, nil
}

func DiskUsage(ctx context.Context, rootDir string) (Usage, error) {
	entries, err := List(ctx, rootDir)
	if err != nil {
		return Usage{}, err
	}
	usage := Usage{}
	seen := make(map[digest.Digest]bool)
	for _, entry := range entries {
		if entry.Reference != "" {
			usage.Tags++
		}
		if !seen[entry.Descriptor.Digest] {
			seen[entry.Descriptor.Digest] = true
			usage.Roots++
		}
	}
	blobs := filepath.Join(rootDir, v1.ImageBlobsDir)
	err = filepath.WalkDir(blobs, func(path string, entry os.DirEntry, walkErr error) error {
		if errors.Is(walkErr, os.ErrNotExist) {
			return nil
		}
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		usage.Bytes += info.Size()
		return ctx.Err()
	})
	return usage, err
}

// Prune removes digest-only roots and lets ORAS garbage collection reclaim
// blobs unreachable from remaining mutable tags.
func Prune(ctx context.Context, rootDir string, dryRun bool) (_ int, retErr error) {
	activity, err := storeactivity.AcquireExclusive(ctx, rootDir)
	if err != nil {
		return 0, fmt.Errorf("acquire prune store activity lease: %w", err)
	}
	defer func() { retErr = errors.Join(retErr, activity.Close()) }()
	return PruneUnderActivityLease(ctx, rootDir, dryRun)
}

// PruneUnderActivityLease prunes a store while the caller holds its exclusive
// storeactivity lease. It allows system prune to lock image and component
// stores in canonical order before touching either one.
func PruneUnderActivityLease(ctx context.Context, rootDir string, dryRun bool) (int, error) {
	dir, err := prepareRoot(rootDir)
	if err != nil {
		return 0, err
	}
	lock := flock.New(filepath.Join(dir, lockFilename))
	locked, err := lock.TryLockContext(ctx, 25*time.Millisecond)
	if err != nil || !locked {
		if err != nil {
			return 0, err
		}
		return 0, errors.New("local OCI store lock not acquired")
	}
	defer func() { _ = lock.Unlock() }()
	data, err := os.ReadFile(filepath.Join(dir, v1.ImageIndexFile))
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var index v1.Index
	if err := json.Unmarshal(data, &index); err != nil {
		return 0, err
	}
	unique := make(map[digest.Digest]v1.Descriptor)
	for _, descriptor := range index.Manifests {
		if descriptor.Annotations[v1.AnnotationRefName] == "" {
			unique[descriptor.Digest] = descriptor
		}
	}
	if dryRun || len(unique) == 0 {
		return len(unique), nil
	}
	store, err := orasoci.NewWithContext(ctx, dir)
	if err != nil {
		return 0, err
	}
	store.AutoGC = true
	for _, descriptor := range unique {
		if err := store.Delete(ctx, descriptor); err != nil && !errors.Is(err, errdef.ErrNotFound) {
			return 0, err
		}
	}
	if err := store.GC(ctx); err != nil {
		return 0, err
	}
	return len(unique), nil
}

func prepareRoot(rootDir string) (string, error) {
	if rootDir == "" {
		return "", errors.New("empty local OCI store path")
	}
	dir, err := filepath.Abs(rootDir)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	dir, err = filepath.EvalSymlinks(dir)
	if err != nil {
		return "", err
	}
	return dir, nil
}

// WriteArchive serializes exactly one rooted OCI graph and atomically installs
// the resulting OCI layout tar at output.
func WriteArchive(ctx context.Context, source content.ReadOnlyStorage, root v1.Descriptor, output string) error {
	if source == nil || output == "" {
		return errors.New("OCI graph source and archive output are required")
	}
	parent, err := filepath.Abs(filepath.Dir(output))
	if err != nil {
		return err
	}
	if err := os.MkdirAll(parent, 0755); err != nil {
		return err
	}
	file, err := os.CreateTemp(parent, ".coopr-oci-*.tar")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(file.Name()) }()
	if err := WriteArchiveTo(ctx, source, root, file); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return os.Rename(file.Name(), output)
}

// CopyGraphLayout copies exactly one verified rooted graph into a caller-owned
// temporary layout. Persistent stores instead use Put and their store locks.
func CopyGraphLayout(ctx context.Context, layout string, source content.ReadOnlyStorage, root v1.Descriptor) error {
	if source == nil {
		return errors.New("nil OCI graph source")
	}
	if root.Digest.Validate() != nil || root.Size < 0 {
		return errors.New("invalid OCI root descriptor")
	}
	target, err := orasoci.NewWithContext(ctx, layout)
	if err != nil {
		return err
	}
	target.AutoSaveIndex = false
	if err := oras.CopyGraph(ctx, source, target, root, oras.CopyGraphOptions{}); err != nil {
		return fmt.Errorf("copy OCI graph: %w", err)
	}
	// ORAS indexes every copied child manifest by digest. The archive contract
	// needs exactly one root descriptor, so write that root after all blobs are
	// copied rather than saving ORAS's in-memory digest tags.
	indexData, err := json.Marshal(v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, Manifests: []v1.Descriptor{root}})
	if err != nil {
		return fmt.Errorf("encode OCI archive index: %w", err)
	}
	if err := os.WriteFile(filepath.Join(layout, v1.ImageIndexFile), indexData, 0o644); err != nil {
		return fmt.Errorf("write OCI archive index: %w", err)
	}
	return nil
}

// WriteArchiveTo serializes exactly one rooted OCI graph as an OCI layout tar
// stream. It does not close writer.
func WriteArchiveTo(ctx context.Context, source content.ReadOnlyStorage, root v1.Descriptor, writer io.Writer) error {
	if source == nil || writer == nil {
		return errors.New("OCI graph source and archive writer are required")
	}
	layout, err := os.MkdirTemp("", ".coopr-oci-layout-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(layout) }()
	if err := CopyGraphLayout(ctx, layout, source, root); err != nil {
		return err
	}
	w := tar.NewWriter(writer)
	err = filepath.WalkDir(layout, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("OCI layout contains non-regular file %s", path)
		}
		name, err := filepath.Rel(layout, path)
		if err != nil {
			return err
		}
		if err := w.WriteHeader(&tar.Header{Name: filepath.ToSlash(name), Mode: 0644, Size: info.Size()}); err != nil {
			return err
		}
		r, err := os.Open(path)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(w, &contextReader{ctx: ctx, r: r})
		closeErr := r.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	})
	if err == nil {
		err = w.Close()
	}
	if err != nil {
		return err
	}
	return ctx.Err()
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}
