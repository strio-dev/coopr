package buildah

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"time"

	"coopr/internal/cache"
	"coopr/internal/imageconfig"
	"coopr/internal/oci"
	"coopr/internal/planner"
	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	upstreamdefine "go.podman.io/buildah/define"
	"go.podman.io/buildah/util"
	"go.podman.io/image/v5/types"
	"go.podman.io/storage"
	"oras.land/oras-go/v2"
	orasoci "oras.land/oras-go/v2/content/oci"
)

const (
	instructionCacheSchema     = "coopr.buildah.instruction-cache.v10"
	instructionCacheBigData    = "coopr-instruction-cache-v10"
	instructionCacheNamePrefix = "coopr.internal/instruction-cache/v10/"
)

// instructionCacheResolvedInput identifies one resolved RUN mount input. The
// entries must be supplied in child/mount order, with exactly one non-empty
// entry per operation child. Identity is executor-owned: stage mounts use the
// source image's OCI rootfs chain ID, context mounts use a content digest,
// and cache mounts use their fully resolved cache ID. Their mutable contents
// deliberately do not invalidate a RUN cache, as in ordinary container builds.
type instructionCacheResolvedInput struct {
	Kind     string `json:"kind"`
	Identity string `json:"identity"`
}

type instructionCacheInput struct {
	ParentRootFS           string
	Logical                *imageconfig.Config
	Operation              planner.Operation
	Platform               v1.Platform
	Isolation              string
	Runtime                string
	Format                 string
	InputDigest            digest.Digest
	ResolvedInputs         []instructionCacheResolvedInput
	ResolvedInputsComplete bool
	RootMetadata           *PackageRootMetadata
	Timestamp              *int64
	SourceDateEpoch        *int64
	RewriteTimestamp       bool
	AddHosts               []string
	RunControls            RunControls
	CompatVolumes          bool
}

type instructionCacheDocument struct {
	Schema           string                          `json:"schema"`
	BuildahVersion   string                          `json:"buildah_version"`
	ParentRootFS     string                          `json:"parent_rootfs"`
	ExecutionConfig  instructionCacheExecutionConfig `json:"execution_config"`
	Operation        planner.Operation               `json:"operation"`
	Platform         v1.Platform                     `json:"platform"`
	Isolation        string                          `json:"isolation"`
	Runtime          string                          `json:"runtime"`
	Format           string                          `json:"format"`
	InputDigest      digest.Digest                   `json:"input_digest,omitempty"`
	ResolvedInputs   []instructionCacheResolvedInput `json:"resolved_inputs,omitempty"`
	RootMetadata     *PackageRootMetadata            `json:"root_metadata,omitempty"`
	Timestamp        *int64                          `json:"timestamp,omitempty"`
	SourceDateEpoch  *int64                          `json:"source_date_epoch,omitempty"`
	RewriteTimestamp bool                            `json:"rewrite_timestamp,omitempty"`
	AddHosts         []string                        `json:"add_hosts,omitempty"`
	RunControls      RunControls                     `json:"run_controls,omitempty"`
	CompatVolumes    bool                            `json:"compat_volumes,omitempty"`
}

// Output-only image metadata is deliberately absent. It is reapplied after a
// cache hit, while these fields can change how Buildah performs filesystem work.
type instructionCacheExecutionConfig struct {
	Env         []string            `json:"Env,omitempty"`
	Hostname    string              `json:"Hostname,omitempty"`
	User        string              `json:"User,omitempty"`
	WorkingDir  string              `json:"WorkingDir,omitempty"`
	Shell       []string            `json:"Shell,omitempty"`
	Volumes     map[string]struct{} `json:"Volumes,omitempty"`
	ArgsEscaped bool                `json:"ArgsEscaped,omitempty"`
}

type instructionCacheRecord struct {
	Schema         string               `json:"schema"`
	CreatedAt      time.Time            `json:"created_at"`
	Key            digest.Digest        `json:"key"`
	ManifestDigest digest.Digest        `json:"manifest_digest"`
	RootMetadata   *PackageRootMetadata `json:"root_metadata,omitempty"`
}

type instructionCacheEntry struct {
	LogicalConfig  json.RawMessage
	BaseImageID    string
	RetainsCaller  *bool
	ImageID        string
	ManifestDigest digest.Digest
	RootMetadata   *PackageRootMetadata
}

type portableInstructionCache struct {
	validateHit func(context.Context, *graphExecutor, *cache.ImageRecord, instructionCacheEntry, *types.SystemContext) error
	readStores  []instructionCacheStore
	writeStores []instructionCacheStore
	stagingDir  string
	candidates  []portableInstructionCandidate
	stats       CacheStats
	cacheTTL    *time.Duration
}

type instructionCacheStore interface {
	LookupImage(context.Context, cache.ImageKey) (*cache.ImageRecord, string, error)
	PutImage(context.Context, cache.ImageKey, cache.ImageRecord, string) (v1.Descriptor, error)
}

type portableInstructionCandidate struct {
	key    cache.ImageKey
	record cache.ImageRecord
	layout string
}

// portableInstructionCacheRelay keeps an isolated worker's deferred OCI graph
// alive in a parent-owned staging directory until the complete build succeeds.
type portableInstructionCacheRelay struct {
	Key            cache.ImageKey    `json:"key"`
	Record         cache.ImageRecord `json:"record"`
	RelativeLayout string            `json:"relative_layout"`
}

var portableInstructionScratchParent = digest.FromString("coopr.strio.dev/instruction-image-cache-parent/scratch/v1")

func cacheStats(c *componentCache) CacheStats {
	if c == nil {
		return CacheStats{}
	}
	return c.stats
}

func portableInstructionCacheStats(c *portableInstructionCache) CacheStats {
	if c == nil {
		return CacheStats{}
	}
	return c.stats
}

func addCacheStats(a, b CacheStats) CacheStats {
	return CacheStats{Hits: a.Hits + b.Hits, Misses: a.Misses + b.Misses, Stored: a.Stored + b.Stored, Skipped: a.Skipped + b.Skipped, Errors: a.Errors + b.Errors}
}

func newPortableInstructionCache(ctx context.Context, options PlanOptions) (*portableInstructionCache, error) {
	bindings, err := cacheBindings(options)
	if err != nil {
		return nil, err
	}
	if len(bindings) == 0 {
		return nil, nil
	}
	dir, err := os.MkdirTemp("", "coopr-instruction-cache-*")
	if err != nil {
		return nil, err
	}
	result := &portableInstructionCache{stagingDir: dir, cacheTTL: options.CacheTTL}
	for _, binding := range bindings {
		var store instructionCacheStore
		if binding.spec.Transport == "oci-layout" {
			store, err = cache.NewLocalStore(ctx, binding.spec.Reference, dir, options.CacheTTL)
		} else {
			store, err = cache.NewRegistryStore(options.Resolver, binding.spec.Reference, dir, options.CacheTTL)
		}
		if err != nil {
			if binding.write || binding.spec.Transport == "registry" {
				return nil, errors.Join(fmt.Errorf("open instruction cache %s:%s: %w", binding.spec.Transport, binding.spec.Reference, err), os.RemoveAll(dir))
			}
			packageCacheWarning("open read-only instruction cache", err)
			result.stats.Errors++
			continue
		}
		if binding.read {
			result.readStores = append(result.readStores, store)
		}
		if binding.write {
			result.writeStores = append(result.writeStores, store)
		}
	}
	return result, nil
}

func (c *portableInstructionCache) close() error {
	if c == nil {
		return nil
	}
	return os.RemoveAll(c.stagingDir)
}

func (c *portableInstructionCache) relayCandidates(destination string) ([]portableInstructionCacheRelay, error) {
	if c == nil || len(c.candidates) == 0 {
		return nil, nil
	}
	if err := cacheRelayRoot(destination); err != nil {
		return nil, err
	}
	relays := make([]portableInstructionCacheRelay, 0, len(c.candidates))
	for _, candidate := range c.candidates {
		relative, err := moveCacheRelayCandidate(candidate.layout, destination, ".instruction-relay-*", "layout")
		if err != nil {
			return nil, err
		}
		relays = append(relays, portableInstructionCacheRelay{Key: candidate.key, Record: candidate.record, RelativeLayout: relative})
	}
	c.candidates = nil
	return relays, nil
}

func (c *portableInstructionCache) acceptRelayedCandidates(relays []portableInstructionCacheRelay) error {
	if len(relays) == 0 {
		return nil
	}
	if c == nil {
		return errors.New("portable instruction cache is disabled")
	}
	for _, relay := range relays {
		keyDigest, err := relay.Key.Digest()
		if err != nil {
			return fmt.Errorf("invalid relayed instruction cache key: %w", err)
		}
		recordDigest, err := relay.Record.Key.Digest()
		if err != nil || recordDigest != keyDigest {
			return errors.New("relayed instruction cache record key differs from candidate")
		}
		if relay.Record.Version != cache.ImageRecordVersion {
			return fmt.Errorf("unsupported relayed instruction cache record %q", relay.Record.Version)
		}
		layout, err := relayedCachePath(c.stagingDir, relay.RelativeLayout, true)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(filepath.Join(layout, "index.json"))
		if err != nil {
			return fmt.Errorf("read relayed instruction cache index: %w", err)
		}
		var index v1.Index
		if err := json.Unmarshal(data, &index); err != nil {
			return fmt.Errorf("decode relayed instruction cache index: %w", err)
		}
		if index.SchemaVersion != 2 {
			return errors.New("relayed instruction cache index has unsupported schema")
		}
		found := false
		foundBase := relay.Record.BaseImage == nil
		for _, root := range index.Manifests {
			same := func(expected v1.Descriptor) bool {
				return root.MediaType == expected.MediaType && root.Digest == expected.Digest && root.Size == expected.Size
			}
			if same(relay.Record.Image) {
				found = true
				if relay.Record.BaseImage != nil && same(*relay.Record.BaseImage) {
					foundBase = true
				}
				continue
			}
			if relay.Record.BaseImage == nil || !same(*relay.Record.BaseImage) {
				return errors.New("relayed instruction cache layout differs from selected image graphs")
			}
			foundBase = true
		}
		if !found || !foundBase {
			return errors.New("relayed instruction cache layout omits selected image")
		}

		c.candidates = append(c.candidates, portableInstructionCandidate{key: relay.Key, record: relay.Record, layout: layout})
	}
	return nil
}

func portableInstructionEligible(_ planner.Operation, lowered Operation, controls RunControls) bool {
	switch operation := lowered.(type) {
	case Run:
		// Network responses, credential contents, and cache-mount contents are
		// deliberately session inputs rather than cache-key inputs, matching
		// conventional container build caching. prepareRunInput supplies stable
		// identities for bind sources and the authored cache-mount scope. CDI
		// devices remain excluded because their resolved host identity and state
		// are not represented by the instruction key.
		return len(operation.Devices) == 0 && len(controls.Devices) == 0
	case WorkDir, Copy, copyFromImageOperation:
		return true
	case Add:
		for _, source := range operation.Sources {
			if (isHTTPAddSource(source) || isGitAddSource(source)) && operation.Checksum == "" {
				return false
			}
		}
		return true
	default:
		return false
	}
}

func portableInstructionParent(parent string) digest.Digest {
	if parent == "" || parent == "empty" {
		return portableInstructionScratchParent
	}
	return digest.Digest(parent)
}

func portableInstructionKey(executor *graphExecutor, input instructionCacheInput, parentIdentity string) (cache.ImageKey, bool, error) {
	if executor == nil || executor.instructionPortableCache == nil {
		return cache.ImageKey{}, false, nil
	}
	parent := portableInstructionParent(parentIdentity)
	if parent.Validate() != nil {
		return cache.ImageKey{}, false, nil
	}
	instruction, cacheable, err := portableInstructionDigest(input, parent)
	if err != nil || !cacheable {
		return cache.ImageKey{}, false, err
	}
	modules, err := cacheModuleVersions()
	if err != nil {
		return cache.ImageKey{}, false, err
	}
	runtimeIdentity := executor.instructionRuntime
	if runtimeIdentity == "" {
		runtimeIdentity = "none"
	}
	semantics, err := componentCacheExecutorSemantics(upstreamdefine.Version, modules, executor.isolation.String(), executor.store.GraphDriverName(), executor.store.GraphOptions(), runtimeIdentity, executor.options.Output.Format)
	if err != nil {
		return cache.ImageKey{}, false, err
	}
	format, err := normalizedOutputFormat(executor.options.Output.Format)
	if err != nil {
		return cache.ImageKey{}, false, err
	}
	key := cache.ImageKey{Instruction: instruction, Parent: parent, Platform: input.Platform, Executor: semantics, Format: format}
	_, err = key.Digest()
	return key, err == nil, err
}

func portableInstructionDigest(input instructionCacheInput, parent digest.Digest) (digest.Digest, bool, error) {
	if parent.Validate() != nil {
		return "", false, nil
	}
	// Use the same ordered OCI layer identity across stores. The scratch
	// sentinel needs a domain-separated digest for the portable record.
	input.ParentRootFS = parent.String()
	return instructionCacheKey(input)
}

func (c *portableInstructionCache) lookup(ctx context.Context, executor *graphExecutor, key cache.ImageKey, localKey digest.Digest, system *types.SystemContext) (instructionCacheEntry, error) {
	return c.lookupValidated(ctx, executor, key, localKey, system, c.validateHit)
}

func (c *portableInstructionCache) lookupValidated(ctx context.Context, executor *graphExecutor, key cache.ImageKey, localKey digest.Digest, system *types.SystemContext, validateHit func(context.Context, *graphExecutor, *cache.ImageRecord, instructionCacheEntry, *types.SystemContext) error) (instructionCacheEntry, error) {
	for _, store := range c.readStores {
		if err := ctx.Err(); err != nil {
			return instructionCacheEntry{}, err
		}
		record, layout, err := store.LookupImage(ctx, key)
		if cancelErr := ctx.Err(); cancelErr != nil {
			return instructionCacheEntry{}, cancelErr
		}
		if err != nil {
			if !errors.Is(err, cache.ErrMiss) {
				packageCacheWarning("read instruction cache", err)
				c.stats.Errors++
			}
			continue
		}
		if record == nil || layout == "" {
			packageCacheWarning("read instruction cache", errors.New("cache returned an incomplete image record"))
			c.stats.Errors++
			continue
		}
		if !cache.RecordFresh(record.CreatedAt, c.cacheTTL, time.Now()) {
			_ = os.RemoveAll(layout)
			continue
		}
		baseImageID := ""
		if record.BaseImage != nil {
			baseImageID, err = ImportSelectedImage(ctx, executor.store, system, layout, *record.BaseImage)
			if err != nil {
				packageCacheWarning("import cached base image", err)
				_ = os.RemoveAll(layout)
				c.stats.Errors++
				continue
			}
		}
		imageID, err := ImportSelectedImage(ctx, executor.store, system, layout, record.Image)
		if err != nil {
			packageCacheWarning("import instruction cache image", err)
			_ = os.RemoveAll(layout)
			c.stats.Errors++
			continue
		}
		var root *PackageRootMetadata
		if len(record.RootMetadata) != 0 {
			root = new(PackageRootMetadata)
			if err := json.Unmarshal(record.RootMetadata, root); err != nil {
				packageCacheWarning("parse instruction cache root metadata", err)
				_ = os.RemoveAll(layout)
				c.stats.Errors++
				continue
			}
			var ok bool
			root, ok = cacheablePackageRootMetadata(root)
			if !ok {
				packageCacheWarning("read instruction cache", errors.New("cached root metadata is not portable"))
				_ = os.RemoveAll(layout)
				c.stats.Errors++
				continue
			}
		}
		entry := instructionCacheEntry{ImageID: imageID, ManifestDigest: record.Image.Digest, RootMetadata: root, LogicalConfig: record.LogicalConfig, BaseImageID: baseImageID, RetainsCaller: record.RetainsCaller}
		if validateHit != nil {
			if err := validateHit(ctx, executor, record, entry, system); err != nil {
				if ctx.Err() != nil {
					_ = os.RemoveAll(layout)
					return instructionCacheEntry{}, ctx.Err()
				}
				packageCacheWarning("validate component image cache", err)
				_ = os.RemoveAll(layout)
				c.stats.Errors++
				continue
			}
		}
		if err := storeInstructionCacheEntrySelectedAt(executor.store, imageID, record.Image.Digest, localKey, root, record.CreatedAt); err != nil {
			_ = os.RemoveAll(layout)
			return instructionCacheEntry{}, err
		}
		// A registry hit seeds earlier stores, normally the caller's local OCI
		// layout, after the graph has been verified and imported successfully.
		var seedErr error
		for _, destination := range c.writeStores {
			if sameCacheStore(destination, store) {
				continue
			}
			if _, err := destination.PutImage(ctx, key, *record, layout); err != nil {
				c.stats.Errors++
				seedErr = errors.Join(seedErr, fmt.Errorf("seed instruction cache: %w", err))
			} else {
				c.stats.Stored++
			}
		}
		if err := os.RemoveAll(layout); err != nil {
			seedErr = errors.Join(seedErr, fmt.Errorf("remove staged instruction cache hit: %w", err))
		}
		if seedErr != nil {
			return instructionCacheEntry{}, seedErr
		}
		c.stats.Hits++
		return entry, nil
	}
	if err := ctx.Err(); err != nil {
		return instructionCacheEntry{}, err
	}
	c.stats.Misses++
	return instructionCacheEntry{}, nil
}

func (c *portableInstructionCache) record(ctx context.Context, executor *graphExecutor, key cache.ImageKey, imageID string, manifest digest.Digest, root *PackageRootMetadata, system *types.SystemContext) {
	c.recordImage(ctx, executor, key, imageID, manifest, root, system, nil, "", nil)
}

func (c *portableInstructionCache) recordImage(ctx context.Context, executor *graphExecutor, key cache.ImageKey, imageID string, manifest digest.Digest, root *PackageRootMetadata, system *types.SystemContext, logical json.RawMessage, baseImageID string, retainsCaller *bool) {
	if c == nil || len(c.writeStores) == 0 {
		return
	}
	dir, err := os.MkdirTemp(c.stagingDir, "candidate-*")
	if err != nil {
		packageCacheWarning("stage instruction cache candidate", err)
		c.stats.Errors++
		return
	}
	layout := filepath.Join(dir, "layout")
	output := executor.options.Output
	output.Path, output.Reference, output.Format = layout, "", key.Format
	result, err := copyStoredOutputSelected(ctx, executor.store, imageID, output, system, optionalDigest(manifest))
	if err != nil {
		packageCacheWarning("export instruction cache candidate", err)
		_ = os.RemoveAll(dir)
		c.stats.Errors++
		return
	}
	descriptor, err := oci.LayoutRoot(result.Layout)
	if err != nil || descriptor.Digest.String() != result.ManifestDigest {
		if err == nil {
			err = errors.New("exported manifest differs from instruction cache candidate")
		}
		packageCacheWarning("verify instruction cache candidate", err)
		_ = os.RemoveAll(dir)
		c.stats.Errors++
		return
	}
	var metadata json.RawMessage
	if root != nil {
		metadata, err = json.Marshal(root)
		if err != nil {
			packageCacheWarning("marshal instruction cache candidate", err)
			_ = os.RemoveAll(dir)
			c.stats.Errors++
			return
		}
	}
	var baseImage *v1.Descriptor
	if baseImageID != "" {
		baseOutput := output
		baseOutput.Path = filepath.Join(dir, "base-layout")
		baseResult, err := copyStoredOutputSelected(ctx, executor.store, baseImageID, baseOutput, system, nil)
		if err != nil {
			packageCacheWarning("export cached base image", err)
			_ = os.RemoveAll(dir)
			c.stats.Errors++
			return
		}
		descriptor, err := oci.LayoutRoot(baseResult.Layout)
		if err == nil {
			source, openErr := orasoci.New(baseResult.Layout)
			if openErr != nil {
				err = openErr
			} else {
				destination, openErr := orasoci.New(layout)
				if openErr != nil {
					err = openErr
				} else {
					err = oras.CopyGraph(ctx, source, destination, descriptor, oras.DefaultCopyGraphOptions)
				}
			}
		}
		if err != nil {
			packageCacheWarning("stage cached base image", err)
			_ = os.RemoveAll(dir)
			c.stats.Errors++
			return
		}
		baseImage = &descriptor
	}
	c.candidates = append(c.candidates, portableInstructionCandidate{key: key, record: cache.ImageRecord{Version: cache.ImageRecordVersion, CreatedAt: time.Now().UTC(), Key: key, Image: descriptor, RootMetadata: metadata, LogicalConfig: logical, BaseImage: baseImage, RetainsCaller: retainsCaller}, layout: layout})
}

func (c *portableInstructionCache) publish(ctx context.Context) error {
	if c == nil {
		return nil
	}
	var exportErr error
	for _, candidate := range c.candidates {
		for _, store := range c.writeStores {
			if ctx.Err() != nil {
				break
			}
			if _, err := store.PutImage(ctx, candidate.key, candidate.record, candidate.layout); err != nil {
				c.stats.Errors++
				exportErr = errors.Join(exportErr, fmt.Errorf("write instruction cache: %w", err))
			} else {
				c.stats.Stored++
			}
		}
		if err := os.RemoveAll(filepath.Dir(candidate.layout)); err != nil {
			exportErr = errors.Join(exportErr, fmt.Errorf("remove staged instruction cache: %w", err))
		}
	}
	c.candidates = nil
	return errors.Join(ctx.Err(), exportErr)
}

// instructionCacheKey covers the ordered filesystem instructions whose inputs
// can be represented using the same conventional cache boundary as Buildah and
// other container builders. COPY and ADD require the digest of the exact input
// stream accepted by Buildah. A RUN with mounts requires a complete, ordered
// resolved-input identity list; mount-free RUN and WORKDIR can be keyed before
// execution. Network responses intentionally do not participate in RUN keys,
// matching normal container build cache semantics.
func instructionCacheKey(input instructionCacheInput) (digest.Digest, bool, error) {
	if input.Operation.Name == "run" && !buildResultCacheEligible(input.AddHosts) {
		return "", false, nil
	}
	switch input.Operation.Name {
	case "run":
		if len(input.Operation.Children) != 0 && !completeResolvedInstructionInputs(input.Operation, input.ResolvedInputs, input.ResolvedInputsComplete) {
			return "", false, nil
		}
	case "copy", "add":
		if input.InputDigest == "" || input.InputDigest.Validate() != nil {
			return "", false, nil
		}
	case "workdir":
	default:
		return "", false, nil
	}
	if input.Logical == nil {
		return "", false, errors.New("instruction cache image config is nil")
	}
	format, err := normalizedOutputFormat(input.Format)
	if err != nil {
		return "", false, err
	}
	if input.Isolation == "" || input.Platform.OS == "" || input.Platform.Architecture == "" {
		return "", false, nil
	}
	if input.Operation.Name == "run" && input.Runtime == "" {
		return "", false, nil
	}
	config, err := instructionExecutionConfig(input.Logical)
	if err != nil {
		return "", false, fmt.Errorf("read instruction cache execution config: %w", err)
	}
	var rootMetadata *PackageRootMetadata
	if input.RootMetadata != nil {
		var ok bool
		rootMetadata, ok = cacheablePackageRootMetadata(input.RootMetadata)
		if !ok {
			return "", false, nil
		}
	}
	document, err := json.Marshal(instructionCacheDocument{
		Schema: instructionCacheSchema, BuildahVersion: upstreamdefine.Version,
		ParentRootFS: input.ParentRootFS, ExecutionConfig: config, Operation: input.Operation,
		Platform: input.Platform, Isolation: input.Isolation, Runtime: input.Runtime, Format: format,
		InputDigest: input.InputDigest, ResolvedInputs: input.ResolvedInputs,
		RootMetadata: rootMetadata, Timestamp: input.Timestamp,
		SourceDateEpoch: input.SourceDateEpoch, RewriteTimestamp: input.RewriteTimestamp,
		AddHosts: slices.Clone(input.AddHosts), RunControls: input.RunControls,
		CompatVolumes: input.CompatVolumes,
	})
	if err != nil {
		return "", false, fmt.Errorf("marshal instruction cache key: %w", err)
	}
	return digest.FromBytes(document), true, nil
}

func instructionExecutionConfig(logical *imageconfig.Config) (instructionCacheExecutionConfig, error) {
	raw, err := logical.MarshalJSON()
	if err != nil {
		return instructionCacheExecutionConfig{}, err
	}
	var document struct {
		Config instructionCacheExecutionConfig `json:"config"`
	}
	if err := json.Unmarshal(raw, &document); err != nil {
		return instructionCacheExecutionConfig{}, err
	}
	return document.Config, nil
}

func completeResolvedInstructionInputs(operation planner.Operation, inputs []instructionCacheResolvedInput, complete bool) bool {
	if !complete || len(inputs) != len(operation.Children) {
		return false
	}
	for _, input := range inputs {
		if input.Kind == "" || input.Identity == "" {
			return false
		}
	}
	return true
}

// instructionRuntimeIdentity captures the exact runtime binary used by
// Buildah. If it cannot be resolved and hashed, RUN caching is disabled while
// execution continues normally.
func instructionRuntimeIdentity(configured string) (string, error) {
	runtimeName := configured
	if runtimeName == "" {
		runtimeName = util.Runtime()
	}
	if local := util.FindLocalRuntime(runtimeName); local != "" {
		runtimeName = local
	}
	path, err := exec.LookPath(runtimeName)
	if err != nil {
		return "", fmt.Errorf("locate OCI runtime %q: %w", runtimeName, err)
	}
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("read OCI runtime %q: %w", path, err)
	}
	defer func() { _ = file.Close() }()
	binary, err := digest.FromReader(file)
	if err != nil {
		return "", fmt.Errorf("hash OCI runtime %q: %w", path, err)
	}
	return path + "@" + binary.String(), nil
}

func findInstructionCache(store storage.Store, key digest.Digest) (string, error) {
	entry, err := findInstructionCacheEntry(store, key, nil)
	return entry.ImageID, err
}

func findInstructionCacheEntry(store storage.Store, key digest.Digest, ttlValues ...*time.Duration) (instructionCacheEntry, error) {
	var ttl *time.Duration
	if len(ttlValues) != 0 {
		ttl = ttlValues[0]
	}
	image, err := store.Image(instructionCacheName(key))
	if err != nil {
		if errors.Is(err, storage.ErrImageUnknown) {
			return instructionCacheEntry{}, nil
		}
		return instructionCacheEntry{}, fmt.Errorf("find instruction cache image: %w", err)
	}
	data, err := store.ImageBigData(image.ID, instructionCacheBigDataName(key))
	if err != nil {
		// A name can outlive partially written metadata after an interrupted or
		// concurrent cache update. Treat that state like any other cache miss.
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, storage.ErrImageUnknown) {
			return instructionCacheEntry{}, nil
		}
		return instructionCacheEntry{}, fmt.Errorf("read instruction cache metadata for %s: %w", image.ID, err)
	}
	var record instructionCacheRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return instructionCacheEntry{}, fmt.Errorf("decode instruction cache metadata for %s: %w", image.ID, err)
	}
	if record.Schema != instructionCacheSchema || record.Key != key {
		return instructionCacheEntry{}, nil
	}
	if !cache.RecordFresh(record.CreatedAt, ttl, time.Now()) {
		return instructionCacheEntry{}, nil
	}
	if record.RootMetadata != nil {
		var ok bool
		record.RootMetadata, ok = cacheablePackageRootMetadata(record.RootMetadata)
		if !ok {
			return instructionCacheEntry{}, nil
		}
	}
	if record.ManifestDigest.Validate() != nil {
		return instructionCacheEntry{}, nil
	}
	return instructionCacheEntry{ImageID: image.ID, ManifestDigest: record.ManifestDigest, RootMetadata: record.RootMetadata}, nil
}

func storeInstructionCache(store storage.Store, imageID string, key digest.Digest) error {
	return storeInstructionCacheEntry(store, imageID, key, nil)
}

func storeInstructionCacheEntry(store storage.Store, imageID string, key digest.Digest, root *PackageRootMetadata) error {
	manifestDigest, err := storedImageDefaultManifestDigest(store, imageID)
	if err != nil {
		return fmt.Errorf("read instruction cache manifest for %s: %w", imageID, err)
	}
	return storeInstructionCacheEntrySelected(store, imageID, manifestDigest, key, root)
}

func storeInstructionCacheEntrySelected(store storage.Store, imageID string, manifestDigest, key digest.Digest, root *PackageRootMetadata) error {
	return storeInstructionCacheEntrySelectedAt(store, imageID, manifestDigest, key, root, time.Now().UTC())
}

func storeInstructionCacheEntrySelectedAt(store storage.Store, imageID string, manifestDigest, key digest.Digest, root *PackageRootMetadata, createdAt time.Time) error {
	if err := manifestDigest.Validate(); err != nil {
		return fmt.Errorf("invalid instruction cache manifest %q: %w", manifestDigest, err)
	}
	var rootMetadata *PackageRootMetadata
	if root != nil {
		var ok bool
		rootMetadata, ok = cacheablePackageRootMetadata(root)
		if !ok {
			return errors.New("instruction cache root metadata is not portable")
		}
	}
	data, err := json.Marshal(instructionCacheRecord{Schema: instructionCacheSchema, CreatedAt: createdAt.UTC(), Key: key, ManifestDigest: manifestDigest, RootMetadata: rootMetadata})
	if err != nil {
		return fmt.Errorf("marshal instruction cache metadata: %w", err)
	}
	if err := store.SetImageBigData(imageID, instructionCacheBigDataName(key), data, nil); err != nil {
		return fmt.Errorf("store instruction cache metadata for %s: %w", imageID, err)
	}
	// AddNames preserves every existing name on this image. The storage image
	// index atomically moves this one private key name if concurrent workers
	// publish the same cache entry, so lookups never require a full image scan.
	if err := store.AddNames(imageID, []string{instructionCacheName(key)}); err != nil {
		return fmt.Errorf("index instruction cache image %s: %w", imageID, err)
	}
	return nil
}

func instructionCacheName(key digest.Digest) string {
	return instructionCacheNamePrefix + key.String()
}

func instructionCacheBigDataName(key digest.Digest) string {
	return instructionCacheBigData + "/" + key.String()
}
