package buildah

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"time"

	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/coopr/internal/cache"
	"github.com/strio-dev/coopr/internal/imageconfig"
	"github.com/strio-dev/coopr/internal/oci"
	"github.com/strio-dev/coopr/internal/planner"
	upstreamdefine "go.podman.io/buildah/define"
	"go.podman.io/buildah/util"
	"go.podman.io/image/v5/types"
	"go.podman.io/storage"
	"oras.land/oras-go/v2"
	orasoci "oras.land/oras-go/v2/content/oci"
)

const (
	instructionCacheSchema     = "coopr.buildah.instruction-cache.v11"
	instructionCacheNamePrefix = "coopr.internal/instruction-cache/v11/"
)

// instructionCacheResolvedInput identifies a resolved RUN input. Mount entries
// follow mount order; device requests use one additional selection entry.
// Identity is executor-owned: stage mounts use the source image's OCI rootfs
// chain ID, context mounts use a content digest, cache mounts use their resolved
// ID, and devices use their selected names. Mutable cache/device contents do not
// invalidate a RUN cache, as in ordinary container builds.
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
	CooprExecution   string                          `json:"coopr_execution"`
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

type instructionCacheEntry struct {
	CommandSet     *bool
	LogicalConfig  json.RawMessage `json:",omitempty"`
	BaseImageID    string
	RetainsCaller  *bool
	ImageID        string
	ManifestDigest digest.Digest
	RootMetadata   *PackageRootMetadata
}

type portableInstructionCache struct {
	readStores     []instructionCacheStore
	readBindingIDs []digest.Digest
	writeStores    []instructionCacheStore
	stagingDir     string
	candidates     []portableInstructionCandidate
	stats          CacheStats
	cacheTTL       *time.Duration
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
	return newPortableImageCache(ctx, options, "instruction")
}

func newPortableImageCache(ctx context.Context, options PlanOptions, name string) (*portableInstructionCache, error) {
	bindings, err := cacheBindings(options)
	if err != nil {
		return nil, err
	}
	if len(bindings) == 0 {
		return nil, nil
	}
	dir, err := os.MkdirTemp("", "coopr-"+name+"-cache-*")
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
			if ctx.Err() != nil {
				return nil, errors.Join(ctx.Err(), os.RemoveAll(dir))
			}
			if binding.write || binding.spec.Transport == "registry" {
				return nil, errors.Join(fmt.Errorf("open %s cache %s:%s: %w", name, binding.spec.Transport, binding.spec.Reference, err), os.RemoveAll(dir))
			}
			packageCacheWarning("open read-only "+name+" cache", err)
			result.stats.Errors++
			continue
		}
		if binding.read {
			result.readStores = append(result.readStores, store)
			result.readBindingIDs = append(result.readBindingIDs, instructionReadBindingID(binding.spec))
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
			baseImageID, err = importInstructionCacheImage(ctx, executor.store, system, layout, *record.BaseImage)
			if err != nil {
				packageCacheWarning("import cached base image", err)
				_ = os.RemoveAll(layout)
				c.stats.Errors++
				continue
			}
		}
		imageID, err := importInstructionCacheImage(ctx, executor.store, system, layout, record.Image)
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
		entry := instructionCacheEntry{ImageID: imageID, ManifestDigest: record.Image.Digest, RootMetadata: root, LogicalConfig: record.LogicalConfig, BaseImageID: baseImageID, RetainsCaller: record.RetainsCaller, CommandSet: record.CommandSet}
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
		if err := retainInstructionCacheImage(executor.store, imageID, localKey); err != nil {
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

// Cache checkpoints promise the exact selected manifest. Native storage keeps
// unpacked layers, so retain compressed representations before the verified
// portable layout is removed; otherwise a later export can change its digest.
func importInstructionCacheImage(ctx context.Context, store storage.Store, system *types.SystemContext, layout string, selected v1.Descriptor) (string, error) {
	imageID, err := ImportSelectedImage(ctx, store, system, layout, selected)
	if err != nil {
		return "", err
	}
	if err := retainStoredLayoutLayers(store, imageID, layout, selected.Digest); err != nil {
		return "", fmt.Errorf("retain selected instruction cache layers: %w", err)
	}
	return imageID, nil
}

func (c *portableInstructionCache) recordImage(ctx context.Context, executor *graphExecutor, key cache.ImageKey, imageID string, manifest digest.Digest, root *PackageRootMetadata, system *types.SystemContext, logical json.RawMessage, baseImageID string, retainsCaller *bool, commandSet *bool) {
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
	c.candidates = append(c.candidates, portableInstructionCandidate{key: key, record: cache.ImageRecord{Version: cache.ImageRecordVersion, CreatedAt: time.Now().UTC(), Key: key, Image: descriptor, RootMetadata: metadata, LogicalConfig: logical, BaseImage: baseImage, RetainsCaller: retainsCaller, CommandSet: commandSet}, layout: layout})
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
	switch input.Operation.Name {
	case "run":
		if len(input.RunControls.Volumes) != 0 {
			return "", false, nil
		}
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
	operation := canonicalInstructionCacheOperation(input.Operation, input.ResolvedInputs, input.ResolvedInputsComplete)
	document, err := json.Marshal(instructionCacheDocument{
		CooprExecution: cooprExecutionRevision,
		Schema:         instructionCacheSchema, BuildahVersion: upstreamdefine.Version,
		ParentRootFS: input.ParentRootFS, ExecutionConfig: config, Operation: operation,
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

// canonicalInstructionCacheOperation is shared by cache keys and persisted
// instruction history. Runtime execution retains the authored operation.
func canonicalInstructionCacheOperation(operation planner.Operation, inputs []instructionCacheResolvedInput, complete bool) planner.Operation {
	if (operation.Name == "copy" || operation.Name == "add") && operation.Properties["from"] != "" {
		// COPY/ADD cacheability requires the exact measured input stream. The
		// delivery selector does not add filesystem semantics to those bytes;
		// in particular, native image IDs can differ for configuration changes
		// that COPY does not inherit. Paths, options and InputDigest stay keyed.
		operation.Properties = maps.Clone(operation.Properties)
		operation.Properties["from"] = "resolved:copy-input"
	}
	if operation.Name == "run" && len(operation.Children) != 0 && completeResolvedInstructionInputs(operation, inputs, complete) {
		// A mounted filesystem does not inherit its source image's config.
		// Its verified resolved identity supersedes temporary storage handles,
		// while every authored path and mount option still participates.
		operation.Children = slices.Clone(operation.Children)
		resolvedIndex := 0
		for index, child := range operation.Children {
			if child.Name != "mount" {
				continue
			}
			for inputs[resolvedIndex].Kind == "devices" {
				resolvedIndex++
			}
			resolved := inputs[resolvedIndex]
			resolvedIndex++
			filesystem := resolved.Kind == "image-rootfs" || resolved.Kind == "artifact-files"
			verified := digest.Digest(resolved.Identity).Validate() == nil || (resolved.Kind == "image-rootfs" && resolved.Identity == "empty")
			if filesystem && verified && child.Properties["from"] != "" {
				child.Properties = maps.Clone(child.Properties)
				child.Properties["from"] = "resolved:" + resolved.Kind + ":" + resolved.Identity
				operation.Children[index] = child
			}
		}
	}
	return operation
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
	if !complete {
		return false
	}
	mounts, deviceRequest := 0, false
	for _, child := range operation.Children {
		switch child.Name {
		case "mount":
			mounts++
		case "device":
			deviceRequest = true
		default:
			return false
		}
	}
	observedMounts, observedDevices := 0, 0
	for _, input := range inputs {
		if input.Kind == "" || input.Identity == "" {
			return false
		}
		if input.Kind == "devices" {
			observedDevices++
		} else {
			observedMounts++
		}
	}
	return observedMounts == mounts && observedDevices <= 1 && (!deviceRequest || observedDevices == 1)
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

// The OCI cache owns identity, restoration metadata and expiry. Native storage
// needs only a retention name; this is not a second cache-record index.
func retainInstructionCacheImage(store storage.Store, imageID string, key digest.Digest) error {
	if err := store.AddNames(imageID, []string{instructionCacheName(key)}); err != nil {
		return fmt.Errorf("retain cached image %s: %w", imageID, err)
	}
	return nil
}

func instructionCacheName(key digest.Digest) string {
	return instructionCacheNamePrefix + key.String()
}
