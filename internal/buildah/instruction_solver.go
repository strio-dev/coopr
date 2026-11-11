package buildah

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/solver"
	"github.com/moby/buildkit/util/compression"
	digest "github.com/opencontainers/go-digest"
	"github.com/strio-dev/coopr/internal/cache"
	"go.podman.io/storage"
	"go.podman.io/storage/pkg/ioutils"
)

const instructionSolverVersion = "coopr.buildah.graph-cache.v1"
const graphResultNamePrefix = "coopr.internal/graph-result/v1/"

// instructionSolverResult always identifies a complete immutable image. ParentLayers
// bounds the operation's history span; it is never a request to graft a layer onto
// the currently mounted image.
type instructionSolverResult struct {
	Entry        instructionCacheEntry `json:"entry"`
	ParentLayers int                   `json:"parent_layers"`
	Boundary     digest.Digest         `json:"boundary,omitempty"`
	CreatedAt    time.Time             `json:"created_at"`
}

func (r *instructionSolverResult) ID() string {
	raw, _ := json.Marshal(struct {
		Version      string
		Manifest     digest.Digest
		Root         *PackageRootMetadata
		ParentLayers int
		Boundary     digest.Digest
	}{instructionSolverVersion, r.Entry.ManifestDigest, r.Entry.RootMetadata, r.ParentLayers, r.Boundary})
	return digest.FromBytes(raw).String()
}
func (r *instructionSolverResult) Sys() any                      { return r }
func (r *instructionSolverResult) Clone() solver.Result          { c := *r; return &c }
func (r *instructionSolverResult) Release(context.Context) error { return nil }

type instructionResultStorage struct {
	ctx   context.Context
	store storage.Store
	dir   string
	ttl   *time.Duration
	lazy  *instructionLazyResults
}

func (s *instructionResultStorage) path(id string) (string, error) {
	d := digest.Digest(id)
	if d.Validate() != nil || d.Algorithm() != digest.SHA256 {
		return "", errors.New("invalid graph result ID")
	}
	return filepath.Join(s.dir, d.Encoded()+".json"), nil
}
func (s *instructionResultStorage) read(id string) (*instructionSolverResult, error) {
	path, err := s.path(id)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var r instructionSolverResult
	if err = json.Unmarshal(raw, &r); err != nil {
		return nil, err
	}
	if r.ID() != id {
		return nil, errors.New("graph result identity differs from metadata")
	}
	if r.ParentLayers < 0 || r.CreatedAt.IsZero() || r.Entry.BaseImageID != "" || len(r.Entry.LogicalConfig) != 0 || r.Entry.CommandSet != nil || r.Entry.RetainsCaller != nil {
		return nil, errors.New("invalid graph result metadata")
	}
	if r.Entry.RootMetadata != nil {
		if _, ok := cacheablePackageRootMetadata(r.Entry.RootMetadata); !ok {
			return nil, errors.New("invalid graph root metadata")
		}
	}
	if r.Entry.ImageID == "" {
		if r.Boundary.Validate() != nil || r.Entry.ManifestDigest != "" || r.ParentLayers != 0 {
			return nil, errors.New("non-source graph result has no image")
		}
	} else {
		if _, err = s.store.Image(r.Entry.ImageID); err != nil {
			return nil, err
		}
		if r.Entry.ManifestDigest.Validate() != nil {
			return nil, errors.New("invalid graph image manifest")
		}
		manifest, err := s.store.ImageBigData(r.Entry.ImageID, storage.ImageDigestManifestBigDataNamePrefix+"-"+r.Entry.ManifestDigest.String())
		if err != nil {
			return nil, err
		}
		if digest.FromBytes(manifest) != r.Entry.ManifestDigest {
			return nil, errors.New("graph result image differs from selected manifest")
		}
	}
	return &r, nil
}
func (s *instructionResultStorage) Save(value solver.Result, created time.Time) (record solver.CacheResult, err error) {
	r, ok := value.Sys().(*instructionSolverResult)
	if !ok {
		return solver.CacheResult{}, errors.New("unexpected native solver result")
	}
	lock, err := s.lockResult(s.ctx, r.ID())
	if err != nil {
		return solver.CacheResult{}, err
	}
	defer func() { err = errors.Join(err, lock.Close()) }()
	return s.writeResult(r, created)
}

// writeResult requires the per-result lock, shared by native saves and portable
// materialization. Equal immutable results retain their newest freshness time.
func (s *instructionResultStorage) writeResult(r *instructionSolverResult, created time.Time) (solver.CacheResult, error) {
	if err := s.ctx.Err(); err != nil {
		return solver.CacheResult{}, err
	}
	c := *r
	c.CreatedAt = created.UTC()
	id := c.ID()
	if previous, err := s.read(id); err == nil && previous.CreatedAt.After(c.CreatedAt) {
		c.CreatedAt = previous.CreatedAt
	}
	path, err := s.path(id)
	if err != nil {
		return solver.CacheResult{}, err
	}
	if c.Entry.ImageID != "" {
		if _, err = s.store.Image(c.Entry.ImageID); err != nil {
			return solver.CacheResult{}, err
		}
		if err = s.store.AddNames(c.Entry.ImageID, []string{graphResultNamePrefix + digest.Digest(id).Encoded()}); err != nil {
			return solver.CacheResult{}, err
		}
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return solver.CacheResult{}, err
	}
	if err := ioutils.AtomicWriteFileWithOpts(path, raw, 0600, &ioutils.AtomicFileWriterOptions{}); err != nil {
		return solver.CacheResult{}, err
	}
	return solver.CacheResult{ID: id, CreatedAt: c.CreatedAt}, nil
}
func (s *instructionResultStorage) Load(ctx context.Context, r solver.CacheResult) (solver.Result, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Records already admitted this result through Exists. Aging out between
	// selection and materialization must not turn a cache miss into a build
	// failure; immutable availability still needs verification here.
	if pending := s.lazy.get(r.ID); pending != nil && !pending.failed.Load() {
		return s.loadPortable(ctx, r.ID)
	}
	return s.read(r.ID)
}

func (s *instructionResultStorage) Exists(ctx context.Context, id string) bool {
	if ctx.Err() != nil {
		return false
	}
	r, err := s.describe(id)
	return err == nil && cache.RecordFresh(r.CreatedAt, s.ttl, time.Now())
}
func (s *instructionResultStorage) LoadRemotes(context.Context, solver.CacheResult, *compression.Config, session.Group) ([]*solver.Remote, error) {
	return nil, nil
}

type instructionSolver struct {
	portable *instructionGraphPortable
	ctx      context.Context
	keys     *instructionGraphKeyOverlay
	results  *instructionResultStorage
	solver   *solver.Solver
	job      *solver.Job
	serial   atomic.Uint64
	mu       sync.Mutex
	retained []solver.CachedResult
}

func newInstructionSolver(ctx context.Context, store storage.Store, ttl *time.Duration) (*instructionSolver, error) {
	dir := filepath.Join(store.GraphRoot(), "coopr-graph-cache-v2")
	if err := os.MkdirAll(filepath.Join(dir, "results"), 0700); err != nil {
		return nil, err
	}
	keys, err := newGraphCacheKeyStorage(ctx, filepath.Join(dir, "keys.db"))
	if err != nil {
		return nil, err
	}
	lazy := newInstructionLazyResults(keys)
	s := &instructionSolver{ctx: ctx, keys: &instructionGraphKeyOverlay{graphCacheKeyStorage: keys, lazy: lazy}, results: &instructionResultStorage{ctx: ctx, store: store, dir: filepath.Join(dir, "results"), ttl: ttl, lazy: lazy}}
	lazy.storage = s.results
	keys.keepResult = func(id string) bool {
		// Age controls reuse, not durable availability. In particular a TTL=0
		// worker must not delete another worker's fresh export associations.
		_, err := s.results.read(id)
		return err == nil
	}
	manager := solver.NewCacheManager(ctx, instructionSolverVersion, s.keys, s.results)
	if err := keys.Err(); err != nil {
		return nil, err
	}
	s.solver = solver.NewSolver(solver.SolverOpt{DefaultCache: manager, ResolveOpFunc: func(v solver.Vertex, _ solver.Builder) (solver.Op, error) { return v.Sys().(*instructionVertex), nil }})
	s.job, err = s.solver.NewJob(fmt.Sprintf("coopr-%d-%d", os.Getpid(), time.Now().UnixNano()))
	if err != nil {
		s.solver.Close()
		return nil, err
	}
	if err := keys.Err(); err != nil {
		_ = s.job.Discard()
		s.solver.Close()
		return nil, err
	}
	return s, nil
}
func (s *instructionSolver) close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	for _, r := range s.retained {
		_ = r.Release(context.Background())
	}
	s.retained = nil
	s.mu.Unlock()
	err := s.job.Discard()
	s.solver.Close()
	return errors.Join(err, s.keys.Err(), s.closePortable())
}

type instructionVertex struct {
	executed  atomic.Bool
	owner     *instructionSolver
	key, dgst digest.Digest
	parent    *instructionVertex
	source    *instructionSolverResult
	completed *instructionSolverResult
	ignore    bool
	name      string
	request   chan instructionExecRequest
}
type instructionExecRequest struct {
	parent *instructionSolverResult
	reply  chan instructionExecReply
}
type instructionExecReply struct {
	result *instructionSolverResult
	err    error
}
type instructionSolveDone struct {
	result solver.CachedResult
	err    error
}
type instructionSolve struct {
	vertex  *instructionVertex
	request *instructionExecRequest
	done    chan instructionSolveDone
	cancel  context.CancelFunc
	result  *instructionSolverResult
}

func (v *instructionVertex) Digest() digest.Digest { return v.dgst }
func (v *instructionVertex) portableDigest() digest.Digest {
	if v.parent == nil {
		return v.key
	}
	return digest.FromString(instructionSolverVersion + "\n" + v.key.String() + "\n" + v.parent.portableDigest().String())
}
func (v *instructionVertex) Sys() any     { return v }
func (v *instructionVertex) Name() string { return v.name }
func (v *instructionVertex) Options() solver.VertexOptions {
	return solver.VertexOptions{IgnoreCache: v.ignore}
}
func (v *instructionVertex) Inputs() []solver.Edge {
	if v.parent == nil {
		return nil
	}
	return []solver.Edge{{Vertex: v.parent}}
}
func (v *instructionVertex) Acquire(context.Context) (solver.ReleaseFunc, error) {
	return func() {}, nil
}
func (v *instructionVertex) CacheMap(context.Context, solver.JobContext, int) (*solver.CacheMap, bool, error) {
	return &solver.CacheMap{Digest: v.key, Deps: make([]struct {
		Selector          digest.Digest
		ComputeDigestFunc solver.ResultBasedCacheFunc
		PreprocessFunc    solver.PreprocessFunc
	}, len(v.Inputs()))}, true, nil
}
func (v *instructionVertex) Exec(ctx context.Context, _ solver.JobContext, in []solver.Result) ([]solver.Result, error) {
	if v.source != nil {
		return []solver.Result{v.source.Clone()}, nil
	}
	v.executed.Store(true)
	req := instructionExecRequest{reply: make(chan instructionExecReply, 1)}
	if len(in) != 1 {
		return nil, errors.New("native instruction has no immutable parent")
	}
	var ok bool
	req.parent, ok = in[0].Sys().(*instructionSolverResult)
	if !ok {
		return nil, errors.New("invalid native instruction parent")
	}
	select {
	case v.request <- req:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	select {
	case reply := <-req.reply:
		if reply.err != nil {
			return nil, reply.err
		}
		return []solver.Result{reply.result}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func (s *instructionSolver) source(key digest.Digest, r instructionSolverResult, force bool) *instructionVertex {
	r.Boundary = key
	v := &instructionVertex{owner: s, key: key, dgst: key, source: &r, ignore: force, name: "Coopr immutable source"}
	if force {
		v.dgst = digest.FromString(key.String() + fmt.Sprint(s.serial.Add(1)))
	}
	return v
}
func (s *instructionSolver) begin(ctx context.Context, parent *instructionVertex, key digest.Digest, name string, force bool) (*instructionSolve, error) {
	parts := []string{instructionSolverVersion, key.String(), parent.Digest().String()}
	v := &instructionVertex{owner: s, parent: parent, key: key, name: name, ignore: force, request: make(chan instructionExecRequest, 1)}
	if force {
		parts = append(parts, fmt.Sprint(s.serial.Add(1)))
	}
	v.dgst = digest.FromString(strings.Join(parts, "\n"))
	if err := s.preparePortable(ctx, v); err != nil {
		return nil, err
	}
	solveCtx, cancel := context.WithCancel(ctx)
	pending := &instructionSolve{vertex: v, done: make(chan instructionSolveDone, 1), cancel: cancel}
	go func() {
		r, err := s.job.Build(solveCtx, solver.Edge{Vertex: v})
		pending.done <- instructionSolveDone{r, err}
	}()
	select {
	case req := <-v.request:
		pending.request = &req
		return pending, nil
	case done := <-pending.done:
		if err := pending.accept(done); err != nil {
			cancel()
			return nil, err
		}
		return pending, nil
	case <-ctx.Done():
		cancel()
		done := <-pending.done
		if done.result != nil {
			_ = done.result.Release(context.Background())
		}
		return nil, ctx.Err()
	}
}
func (p *instructionSolve) accept(done instructionSolveDone) error {
	if done.err != nil {
		return done.err
	}
	r, ok := done.result.Sys().(*instructionSolverResult)
	if !ok {
		_ = done.result.Release(context.Background())
		return errors.New("unexpected solver result")
	}
	p.result = r
	p.vertex.completed = r
	p.vertex.owner.stagePortable(p.vertex, r)
	p.vertex.owner.mu.Lock()
	p.vertex.owner.retained = append(p.vertex.owner.retained, done.result)
	p.vertex.owner.mu.Unlock()
	return p.vertex.owner.keys.Err()
}
func (p *instructionSolve) finish(r *instructionSolverResult) error {
	if p.request == nil {
		return nil
	}
	p.request.reply <- instructionExecReply{result: r}
	p.request = nil
	done := <-p.done
	p.cancel()
	return p.accept(done)
}
func (p *instructionSolve) abort() {
	if p == nil {
		return
	}
	p.cancel()
	if p.request != nil {
		done := <-p.done
		if done.result != nil {
			_ = done.result.Release(context.Background())
		}
		p.request = nil
	}
}

// Logical operation identity is independent of generated physical parent layers.
// Source/barrier vertices carry those layers; dependency links carry normal parents.
func instructionGraphOperationKey(input instructionCacheInput) (digest.Digest, bool, error) {
	input.ParentRootFS = ""
	input.RootMetadata = nil
	key, eligible, err := instructionCacheKey(input)
	if err != nil || !eligible {
		return key, eligible, err
	}
	return digest.FromString(instructionSolverVersion + "\n" + key.String()), true, nil
}

// History restoration can rebind different physical parents with the same
// layer shape. A different layer count is a conservative miss until restoration
// supports a complete authored operation/history tape across such boundaries.
func instructionGraphRestorationKey(operation digest.Digest, parentLayers int) digest.Digest {
	return digest.FromString(fmt.Sprintf("%s/history-shape/v1\n%s\n%d", instructionSolverVersion, operation, parentLayers))
}
