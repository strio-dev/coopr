package buildah

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime/debug"
	"strings"
	"time"

	"coopr/internal/cache"
	"coopr/internal/imageconfig"
	"coopr/internal/planner"
	"coopr/internal/stateidentity"
	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"go.podman.io/buildah/define"
	"go.podman.io/image/v5/types"
)

const portableCacheLoweringVersion = "coopr-buildah-component-image-v3"

type componentCache struct {
	images      *portableInstructionCache
	readStores  []cache.Store
	writeStores []cache.Store
	stagingDir  string
	stats       CacheStats
	cacheTTL    *time.Duration
}

// componentCacheRelay is the process-safe description of one deferred cache
// candidate. RelativePath is rooted in the receiving cache's staging
// directory, so an isolated stage never returns a path owned by its disposable
// worker directory.
type componentCacheRelay struct {
	Image portableInstructionCacheRelay `json:"image"`
}

// CacheStats counts optional portable-cache decisions for one build.
type CacheStats struct {
	Hits    int `json:"hits"`
	Misses  int `json:"misses"`
	Stored  int `json:"stored"`
	Skipped int `json:"skipped"`
	Errors  int `json:"errors"`
}

func newComponentCache(ctx context.Context, options PlanOptions) (*componentCache, error) {
	bindings, err := cacheBindings(options)
	if err != nil {
		return nil, err
	}
	if len(bindings) == 0 {
		return nil, nil
	}
	if err := validateComponentCachePath(options); err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", "coopr-component-cache-*")
	if err != nil {
		return nil, fmt.Errorf("create component cache staging: %w", err)
	}
	result := &componentCache{stagingDir: dir, cacheTTL: options.CacheTTL}
	for _, binding := range bindings {
		var store cache.Store
		if binding.spec.Transport == "oci-layout" {
			store, err = cache.NewLocalStore(ctx, binding.spec.Reference, dir, options.CacheTTL)
		} else {
			store, err = cache.NewRegistryStore(options.Resolver, binding.spec.Reference, dir, options.CacheTTL)
		}
		if err != nil {
			if ctx.Err() != nil {
				_ = os.RemoveAll(dir)
				return nil, ctx.Err()
			}
			if binding.write || binding.spec.Transport == "registry" {
				return nil, errors.Join(fmt.Errorf("open component cache %s:%s: %w", binding.spec.Transport, binding.spec.Reference, err), os.RemoveAll(dir))
			}
			packageCacheWarning("open read-only component cache", err)
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

func validateComponentCachePath(options PlanOptions) error {
	bindings, err := cacheBindings(options)
	if err != nil {
		return err
	}
	for _, binding := range bindings {
		if binding.spec.Transport == "oci-layout" {
			if err := validateComponentCachePathValue(options, binding.spec.Reference); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateComponentCachePathValue(options PlanOptions, cacheDir string) error {
	if !filepath.IsAbs(cacheDir) {
		return errors.New("component cache layout path must be absolute")
	}
	cachePath, err := canonicalStorePath(cacheDir)
	if err != nil {
		return fmt.Errorf("resolve component cache layout path: %w", err)
	}
	protectedPaths := []struct{ name, path string }{
		{"execution graph", options.Store.GraphRoot},
		{"execution run root", options.Store.RunRoot},
		{"output", options.Output.Path},
	}
	if options.Resolver != nil {
		protectedPaths = append(protectedPaths,
			struct{ name, path string }{"component store", options.Resolver.ComponentStoreDir()},
		)
	}
	for _, protected := range protectedPaths {
		if protected.path == "" {
			continue
		}
		path, err := canonicalStorePath(protected.path)
		if err != nil {
			return fmt.Errorf("resolve %s path: %w", protected.name, err)
		}
		if pathsOverlap(cachePath, path) {
			return fmt.Errorf("component cache layout overlaps %s", protected.name)
		}
	}
	if options.ContextDir != "" {
		contextPath, err := canonicalStorePath(options.ContextDir)
		if err != nil {
			return fmt.Errorf("resolve build context path: %w", err)
		}
		if pathContains(cachePath, contextPath) {
			return errors.New("component cache layout contains the build context")
		}
	}
	return nil
}

func (c *componentCache) close() error {
	if c == nil {
		return nil
	}
	return os.RemoveAll(c.stagingDir)
}

func (c *componentCache) relayCandidates(destination string) ([]componentCacheRelay, error) {
	if c == nil {
		return nil, nil
	}
	images, err := c.imageCache().relayCandidates(destination)
	if err != nil {
		return nil, err
	}
	relays := make([]componentCacheRelay, len(images))
	for i := range images {
		relays[i].Image = images[i]
	}
	return relays, nil
}

func (c *componentCache) acceptRelayedCandidates(relays []componentCacheRelay) error {
	if len(relays) == 0 {
		return nil
	}
	if c == nil {
		return errors.New("component cache is disabled")
	}
	images := make([]portableInstructionCacheRelay, len(relays))
	for i := range relays {
		images[i] = relays[i].Image
	}
	return c.imageCache().acceptRelayedCandidates(images)
}

func cacheRelayRoot(root string) error {
	if root == "" {
		return errors.New("cache relay root is empty")
	}
	info, err := os.Stat(root)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("cache relay root is not a directory")
	}
	return nil
}

func moveCacheRelayCandidate(source, destination, pattern, name string) (string, error) {
	if err := cacheRelayRoot(destination); err != nil {
		return "", err
	}
	dir, err := os.MkdirTemp(destination, pattern)
	if err != nil {
		return "", err
	}
	target := filepath.Join(dir, name)
	if err := os.Rename(source, target); err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	relative, err := filepath.Rel(destination, target)
	if err != nil {
		return "", err
	}
	return relative, nil
}

func relayedCachePath(root, relative string, directory bool) (string, error) {
	if err := cacheRelayRoot(root); err != nil {
		return "", err
	}
	clean := filepath.Clean(relative)
	if relative == "" || filepath.IsAbs(relative) || clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", errors.New("invalid cache relay path")
	}
	path := filepath.Join(root, clean)
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	resolvedPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	within, err := filepath.Rel(resolvedRoot, resolvedPath)
	if err != nil || within == ".." || strings.HasPrefix(within, ".."+string(filepath.Separator)) {
		return "", errors.New("cache relay path escapes staging directory")
	}
	info, err := os.Stat(resolvedPath)
	if err != nil {
		return "", err
	}
	if info.IsDir() != directory {
		return "", errors.New("cache relay path has unexpected type")
	}
	return resolvedPath, nil
}

func componentCacheEligible(resolved *ResolvedComponentPlan, controls RunControls) bool {
	if resolved == nil || resolved.Plan == nil || len(resolved.Plan.Outputs) != 1 {
		return false
	}
	inputs := make(map[string]bool)
	outputFound := false
	for _, stage := range resolved.Plan.Stages {
		if stage.ID == resolved.Plan.Outputs[0] {
			outputFound = true
		}
		inputs[strings.ToLower(stage.ID)] = true
		if stage.Name != "" {
			inputs[strings.ToLower(stage.Name)] = true
		}
		if stage.DeferredImageSource || stage.SourceContext != "" || stage.DynamicBaseStage != "" {
			return false
		}
		if stage.Kind == "package-input" {
			if _, ok := resolved.PackageInputs[stage.ID]; !ok {
				return false
			}
		}
	}
	if !outputFound {
		return false
	}
	for _, stage := range resolved.Plan.Stages {
		for _, operation := range stage.Operations {
			switch operation.Name {
			case "layer":
				if operation.LayerBoundary != "begin" && operation.LayerBoundary != "end" {
					return false
				}
			case "arg", "env", "label", "user", "cmd", "entrypoint", "shell", "stopsignal", "expose", "volume", "maintainer", "workdir":
				if len(operation.Children) != 0 {
					return false
				}
			case "onbuild", "healthcheck":
			// Deferred/image configuration only; body validation belongs to the public parser.
			case "run":
				if len(controls.Devices) != 0 || !componentCacheRunEligible(operation, inputs) {
					return false
				}
			case "copy", "add":
				if len(operation.Children) != 0 || !inputs[strings.ToLower(operation.Properties["from"])] {
					return false
				}
			default:
				return false
			}
		}
	}
	return true
}

func componentCacheRunEligible(operation planner.Operation, packages map[string]bool) bool {
	if network := operation.Properties["network"]; network != "" && network != "default" && network != "none" {
		return false
	}
	if operation.Properties["security"] == "insecure" || hasRunDeviceOptions(operation) {
		return false
	}
	for _, child := range operation.Children {
		if child.Name != "mount" || len(child.Arguments) != 1 {
			return false
		}
		switch child.Arguments[0] {
		case "bind":
			if !packages[strings.ToLower(child.Properties["from"])] {
				return false
			}
		case "cache", "tmpfs", "secret", "ssh":
		default:
			return false
		}
	}
	return true
}

func componentCacheNeedsRuntime(resolved *ResolvedComponentPlan) bool {
	for _, stage := range resolved.Plan.Stages {
		for _, operation := range stage.Operations {
			if operation.Name == "run" {
				return true
			}
		}
	}
	return false
}

// Buildah checkpoints write their own wall-clock created timestamp. It is
// executor provenance, not a component input. The complete config still goes
// into the snapshot and final image; only the cache input identity omits it.
func cacheIdentityConfig(raw json.RawMessage) (json.RawMessage, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return nil, err
	}
	delete(object, "created")
	return json.Marshal(object)
}

func (c *componentCache) key(executor *graphExecutor, input stateidentity.Identity, resolved *ResolvedComponentPlan, parameters map[string]string, platform v1.Platform) (cache.Key, error) {
	packages := make(map[string]v1.Descriptor, len(resolved.PackageInputs))
	for stageID, pkg := range resolved.PackageInputs {
		packages[stageID] = pkg.Descriptor
	}
	inputs := componentCacheInputs(resolved)
	policy, err := json.Marshal(struct {
		Timestamp        *int64 `json:"timestamp,omitempty"`
		SourceDateEpoch  *int64 `json:"source_date_epoch,omitempty"`
		RewriteTimestamp bool   `json:"rewrite_timestamp,omitempty"`
	}{executor.options.Timestamp, executor.options.SourceDateEpoch, executor.options.RewriteTimestamp})
	if err != nil {
		return cache.Key{}, err
	}
	inputs["timestamp-policy"] = digest.FromBytes(policy)
	executionOptions, err := buildExecutionOptionsDigest(resolved.Plan.Stages, executor.options.AddHosts)
	if err != nil {
		return cache.Key{}, err
	}
	inputs["execution-options"] = executionOptions
	runControls, err := cacheRunControlsDigest(executor.options.RunControls)
	if err != nil {
		return cache.Key{}, err
	}
	inputs["run-controls"] = runControls
	mode, err := json.Marshal(struct {
		NoLayers      bool
		CompatVolumes bool
	}{executor.options.Lifecycle.NoLayers, executor.options.Lifecycle.CompatVolumes})
	if err != nil {
		return cache.Key{}, err
	}
	inputs["execution-mode"] = digest.FromBytes(mode)
	modules, err := cacheModuleVersions()
	if err != nil {
		return cache.Key{}, err
	}
	executorSemantics, err := componentCacheExecutorSemantics(
		define.Version, modules, executor.isolation.String(), executor.store.GraphDriverName(), executor.store.GraphOptions(), executor.instructionRuntime, executor.options.Output.Format,
	)
	if err != nil {
		return cache.Key{}, err
	}
	key := cache.Key{
		Input: input, Component: resolved.Identity, Parameters: parameters, Packages: packages, Inputs: inputs,
		Platform: platform, Executor: executorSemantics,
		Frontend: "coopr-definition-v1", Lowering: portableCacheLoweringVersion,
	}
	_, err = key.Digest()
	return key, err
}

func cacheRunControlsDigest(controls RunControls) (digest.Digest, error) {
	data, err := json.Marshal(controls)
	if err != nil {
		return "", err
	}
	return digest.FromBytes(data), nil
}

func componentCacheInputs(resolved *ResolvedComponentPlan) map[string]digest.Digest {
	inputs := make(map[string]digest.Digest, len(resolved.PackageInputs)+len(resolved.SelectedBases))
	for stageID, pkg := range resolved.PackageInputs {
		inputs["package-config-"+stageID] = digest.FromBytes(pkg.Config)
	}
	for key, selected := range resolved.SelectedBases {
		// The reference and platform identify the graph input while the value
		// records the immutable manifest selected for this invocation. Hash the
		// identity so arbitrary image references remain valid cache input names.
		identity := digest.FromString(key.Reference + "\x00" + key.Platform).Encoded()
		inputs["external-base-"+identity] = selected.Selected.Digest
	}
	return inputs
}

func componentCacheExecutorSemantics(buildahVersion, modules, isolation, driver string, driverOptions []string, runtimeIdentity, outputFormat string) (string, error) {
	format, err := normalizedOutputFormat(outputFormat)
	if err != nil {
		return "", err
	}
	semantics, err := json.Marshal(struct {
		Buildah       string   `json:"buildah"`
		Modules       string   `json:"modules"`
		Isolation     string   `json:"isolation"`
		Driver        string   `json:"driver"`
		DriverOptions []string `json:"driver_options,omitempty"`
		Runtime       string   `json:"runtime"`
		OutputFormat  string   `json:"output_format"`
	}{buildahVersion, modules, isolation, driver, driverOptions, runtimeIdentity, format})
	if err != nil {
		return "", err
	}
	return "buildah-" + digest.FromBytes(semantics).Encoded(), nil
}

// Buildah, containers/storage, and containers/image can independently change
// archive, import, or execution semantics. Keep all three in portable keys.
func cacheModuleVersions() (string, error) {
	paths := []string{"go.podman.io/buildah", "go.podman.io/storage", "go.podman.io/image/v5"}
	versions := make(map[string]string, len(paths))
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "", errors.New("go build information is required for portable cache keys")
	}
	for _, dependency := range info.Deps {
		for _, path := range paths {
			if dependency.Path == path {
				version := dependency.Version
				if dependency.Replace != nil {
					if dependency.Replace.Version == "" {
						return "", fmt.Errorf("unversioned replacement for %s cannot use portable cache", path)
					}
					version = dependency.Replace.Path + "@" + dependency.Replace.Version
				}
				versions[path] = version
			}
		}
	}
	parts := make([]string, 0, len(paths)+1)
	parts = append(parts, info.GoVersion)
	for _, path := range paths {
		if versions[path] == "" {
			return "", fmt.Errorf("missing %s build version for portable cache key", path)
		}
		parts = append(parts, path+"@"+versions[path])
	}
	return strings.Join(parts, "/"), nil
}

// lookup treats unavailable or corrupt optional cache records as misses. The
// restored state is re-observed before a hit can replace component execution.
func (c *componentCache) imageCache() *portableInstructionCache {
	if c.images != nil {
		return c.images
	}
	c.images = &portableInstructionCache{stagingDir: c.stagingDir, cacheTTL: c.cacheTTL, validateHit: validateComponentImageCacheHit}
	for _, store := range c.readStores {
		if images, ok := store.(instructionCacheStore); ok {
			c.images.readStores = append(c.images.readStores, images)
		}
	}
	for _, store := range c.writeStores {
		if images, ok := store.(instructionCacheStore); ok {
			c.images.writeStores = append(c.images.writeStores, images)
		}
	}
	return c.images
}

func componentImageCacheKey(key cache.Key, format string) (cache.ImageKey, error) {
	identity, err := key.Digest()
	if err != nil {
		return cache.ImageKey{}, err
	}
	normalized, err := normalizedOutputFormat(format)
	if err != nil {
		return cache.ImageKey{}, err
	}
	return cache.ImageKey{Instruction: identity, Parent: key.Input.State, Platform: key.Platform, Executor: key.Executor, Format: normalized}, nil
}

// Component results use verified whole-image graphs, never flattened filesystem
// snapshots. The key includes the caller's selected manifest to retain its exact
// parent layers even when another caller has equivalent filesystem contents.
func (c *componentCache) lookup(ctx context.Context, executor *graphExecutor, key cache.Key, system *types.SystemContext, caller componentCacheCaller) (instructionCacheEntry, *imageconfig.Config, error) {
	if err := ctx.Err(); err != nil {
		return instructionCacheEntry{}, nil, err
	}
	imageKey, err := componentImageCacheKey(key, executor.options.Output.Format)
	if err != nil {
		return instructionCacheEntry{}, nil, err
	}
	localKey, err := imageKey.Digest()
	if err != nil {
		return instructionCacheEntry{}, nil, err
	}
	images := c.imageCache()
	before := images.stats
	entry, err := images.lookupValidated(ctx, executor, imageKey, localKey, system, func(ctx context.Context, executor *graphExecutor, record *cache.ImageRecord, entry instructionCacheEntry, system *types.SystemContext) error {
		if err := validateComponentImageCacheHit(ctx, executor, record, entry, system); err != nil {
			return err
		}
		if *entry.RetainsCaller {
			if !caller.RetainsCallerAllowed {
				return errors.New("cached component claims caller ancestry outside selected output contract")
			}
			parentRaw, err := packageImageConfigSelected(ctx, executor.store, caller.ImageID, system, caller.Manifest)
			if err != nil {
				return err
			}
			baseRaw, err := packageImageConfigSelected(ctx, executor.store, entry.BaseImageID, system, record.BaseImage.Digest)
			if err != nil {
				return err
			}
			var parent, base v1.Image
			if err := json.Unmarshal(parentRaw, &parent); err != nil {
				return err
			}
			if err := json.Unmarshal(baseRaw, &base); err != nil {
				return err
			}
			if len(parent.RootFS.DiffIDs) > len(base.RootFS.DiffIDs) {
				return errors.New("cached component discarded caller image chain")
			}
			for i, id := range parent.RootFS.DiffIDs {
				if id != base.RootFS.DiffIDs[i] {
					return errors.New("cached component discarded caller image chain")
				}
			}
		}
		return nil
	})
	c.stats = addCacheStats(c.stats, cacheStatsDifference(images.stats, before))
	if err != nil || entry.ImageID == "" {
		return entry, nil, err
	}
	raw, err := packageImageConfigSelected(ctx, executor.store, entry.ImageID, system, entry.ManifestDigest)
	if err != nil {
		return instructionCacheEntry{}, nil, err
	}
	config, err := imageconfig.Parse(entry.LogicalConfig)
	if err != nil {
		return instructionCacheEntry{}, nil, err
	}
	if err := config.AdoptExecutorProvenance(raw); err != nil {
		return instructionCacheEntry{}, nil, err
	}
	return entry, config, nil
}

func cacheStatsDifference(after, before CacheStats) CacheStats {
	return CacheStats{Hits: after.Hits - before.Hits, Misses: after.Misses - before.Misses, Stored: after.Stored - before.Stored, Skipped: after.Skipped - before.Skipped, Errors: after.Errors - before.Errors}
}

func (c *componentCache) record(ctx context.Context, executor *graphExecutor, key cache.Key, imageID string, manifest digest.Digest, config *imageconfig.Config, root *PackageRootMetadata, system *types.SystemContext, baseImageID string, retainsCaller bool) {
	imageKey, err := componentImageCacheKey(key, executor.options.Output.Format)
	if err != nil {
		packageCacheWarning("prepare component image cache", err)
		c.stats.Skipped++
		return
	}
	raw, err := config.MarshalJSON()
	if err != nil {
		packageCacheWarning("record component image config", err)
		c.stats.Errors++
		return
	}
	images := c.imageCache()
	before := images.stats
	images.recordImage(ctx, executor, imageKey, imageID, manifest, root, system, raw, baseImageID, &retainsCaller)
	c.stats = addCacheStats(c.stats, cacheStatsDifference(images.stats, before))
}

func (c *componentCache) publish(ctx context.Context) error {
	if c == nil {
		return ctx.Err()
	}
	images := c.imageCache()
	before := images.stats
	err := images.publish(ctx)
	c.stats = addCacheStats(c.stats, cacheStatsDifference(images.stats, before))
	return err
}

// Native OCI omits Docker-only extension fields. The sidecar may preserve those,
// but cannot override runtime fields represented by the selected image itself.
func componentCachedConfigMatchesImage(logical, native []byte) error {
	var left, right builderConfigDocument
	if err := json.Unmarshal(logical, &left); err != nil {
		return err
	}
	if err := json.Unmarshal(native, &right); err != nil {
		return err
	}
	normalize := func(config *builderRuntimeConfig) {
		config.Shell = nil
		config.Hostname = ""
		if len(config.Env) == 0 {
			config.Env = nil
		}
		if len(config.Cmd) == 0 {
			config.Cmd = nil
		}
		if len(config.Entrypoint) == 0 {
			config.Entrypoint = nil
		}
		if len(config.Labels) == 0 {
			config.Labels = nil
		}
		if len(config.Volumes) == 0 {
			config.Volumes = nil
		}
		if len(config.ExposedPorts) == 0 {
			config.ExposedPorts = nil
		}
	}
	var leftObject, rightObject struct {
		Config map[string]json.RawMessage `json:"config"`
	}
	if err := json.Unmarshal(logical, &leftObject); err != nil {
		return err
	}
	if err := json.Unmarshal(native, &rightObject); err != nil {
		return err
	}
	for _, name := range []string{"Shell", "OnBuild", "Healthcheck"} {
		raw, exists := rightObject.Config[name]
		if !exists || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			continue
		}
		decode := func(raw json.RawMessage) (any, error) {
			var result any
			decoder := json.NewDecoder(bytes.NewReader(raw))
			decoder.UseNumber()
			err := decoder.Decode(&result)
			return result, err
		}
		expected, err := decode(raw)
		if err != nil {
			return err
		}
		actual, err := decode(leftObject.Config[name])
		if err != nil {
			return fmt.Errorf("cached logical %s is missing: %w", name, err)
		}
		if !reflect.DeepEqual(actual, expected) {
			return fmt.Errorf("cached logical %s disagrees with selected image configuration", name)
		}
	}
	// Buildah adds its identity label at commit; it is not an authored runtime change.
	if _, authored := left.Config.Labels["io.buildah.version"]; !authored && right.Config.Labels["io.buildah.version"] == define.Version {
		delete(right.Config.Labels, "io.buildah.version")
	}
	normalize(&left.Config)
	normalize(&right.Config)
	if !reflect.DeepEqual(left, right) {
		for i := 0; i < reflect.TypeOf(left.Config).NumField(); i++ {
			if !reflect.DeepEqual(reflect.ValueOf(left.Config).Field(i).Interface(), reflect.ValueOf(right.Config).Field(i).Interface()) {
				return fmt.Errorf("cached logical %s disagrees with selected image runtime configuration", reflect.TypeOf(left.Config).Field(i).Name)
			}
		}
		return errors.New("cached logical author disagrees with selected image runtime configuration")
	}
	return nil
}

func validateComponentImageCacheHit(ctx context.Context, executor *graphExecutor, record *cache.ImageRecord, entry instructionCacheEntry, system *types.SystemContext) error {
	if entry.RootMetadata == nil || entry.RetainsCaller == nil || len(entry.LogicalConfig) == 0 || record.BaseImage == nil || entry.BaseImageID == "" {
		return errors.New("cached component image is missing root metadata, logical configuration, executed lineage or effective base image")
	}
	logical, err := imageconfig.Parse(entry.LogicalConfig)
	if err != nil {
		return err
	}
	raw, err := packageImageConfigSelected(ctx, executor.store, entry.ImageID, system, entry.ManifestDigest)
	if err != nil {
		return err
	}
	if err := componentCachedConfigMatchesImage(entry.LogicalConfig, raw); err != nil {
		return err
	}
	if err := logical.AdoptExecutorProvenance(raw); err != nil {
		return err
	}
	baseRaw, err := packageImageConfigSelected(ctx, executor.store, entry.BaseImageID, system, record.BaseImage.Digest)
	if err != nil {
		return err
	}
	var selected, base v1.Image
	if err := json.Unmarshal(raw, &selected); err != nil {
		return err
	}
	if err := json.Unmarshal(baseRaw, &base); err != nil {
		return err
	}
	if base.OS != selected.OS || base.Architecture != selected.Architecture || base.Variant != selected.Variant || len(base.RootFS.DiffIDs) > len(selected.RootFS.DiffIDs) {
		return errors.New("cached effective base image is outside selected image lineage")
	}
	for i, id := range base.RootFS.DiffIDs {
		if id != selected.RootFS.DiffIDs[i] {
			return errors.New("cached effective base image is outside selected image lineage")
		}
	}
	return nil
}

type componentCacheCaller struct {
	ImageID              string
	Manifest             digest.Digest
	RetainsCallerAllowed bool
}
