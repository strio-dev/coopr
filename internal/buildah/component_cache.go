package buildah

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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

const portableCacheLoweringVersion = "coopr-buildah-component-v2"

type componentCache struct {
	readStores  []cache.Store
	writeStores []cache.Store
	stagingDir  string
	candidates  []componentCacheCandidate
	stats       CacheStats
	cacheTTL    *time.Duration
}

type componentCacheCandidate struct {
	key    cache.Key
	record cache.Record
	path   string
}

// componentCacheRelay is the process-safe description of one deferred cache
// candidate. RelativePath is rooted in the receiving cache's staging
// directory, so an isolated stage never returns a path owned by its disposable
// worker directory.
type componentCacheRelay struct {
	Key          cache.Key    `json:"key"`
	Record       cache.Record `json:"record"`
	RelativePath string       `json:"relative_path,omitempty"`
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
	if c == nil || len(c.candidates) == 0 {
		return nil, nil
	}
	if err := cacheRelayRoot(destination); err != nil {
		return nil, err
	}
	relays := make([]componentCacheRelay, 0, len(c.candidates))
	for _, candidate := range c.candidates {
		relay := componentCacheRelay{Key: candidate.key, Record: candidate.record}
		if candidate.path != "" {
			relative, err := moveCacheRelayCandidate(candidate.path, destination, ".component-relay-*", "snapshot.tar")
			if err != nil {
				return nil, err
			}
			relay.RelativePath = relative
		}
		relays = append(relays, relay)
	}
	c.candidates = nil
	return relays, nil
}

func (c *componentCache) acceptRelayedCandidates(relays []componentCacheRelay) error {
	if len(relays) == 0 {
		return nil
	}
	if c == nil {
		return errors.New("component cache is disabled")
	}
	for _, relay := range relays {
		keyDigest, err := relay.Key.Digest()
		if err != nil {
			return fmt.Errorf("invalid relayed component cache key: %w", err)
		}
		recordDigest, err := relay.Record.Key.Digest()
		if err != nil || recordDigest != keyDigest {
			return errors.New("relayed component cache record key differs from candidate")
		}
		if relay.Record.Version != cache.RecordVersion {
			return fmt.Errorf("unsupported relayed component cache record %q", relay.Record.Version)
		}
		if relay.Record.ChangedFS != (relay.RelativePath != "") || relay.Record.ChangedFS != (relay.Record.Snapshot != nil) {
			return errors.New("relayed component cache snapshot differs from record")
		}
		path := ""
		if relay.RelativePath != "" {
			path, err = relayedCachePath(c.stagingDir, relay.RelativePath, false)
			if err != nil {
				return err
			}
			file, err := os.Open(path)
			if err != nil {
				return err
			}
			actual, digestErr := digest.FromReader(file)
			closeErr := file.Close()
			if err := errors.Join(digestErr, closeErr); err != nil {
				return err
			}
			info, err := os.Stat(path)
			if err != nil {
				return err
			}
			if actual != relay.Record.Snapshot.Descriptor.Digest || info.Size() != relay.Record.Snapshot.Descriptor.Size {
				return errors.New("relayed component cache snapshot differs from descriptor")
			}
		}
		c.candidates = append(c.candidates, componentCacheCandidate{key: relay.Key, record: relay.Record, path: path})
	}
	return nil
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
	outputID := resolved.Plan.Outputs[0]
	var output *planner.Stage
	packages := make(map[string]bool)
	for i := range resolved.Plan.Stages {
		stage := &resolved.Plan.Stages[i]
		if stage.ID == outputID {
			output = stage
			continue
		}
		if stage.Kind != "package-input" || stage.Name == "" {
			return false
		}
		if _, ok := resolved.PackageInputs[stage.ID]; !ok {
			return false
		}
		packages[strings.ToLower(stage.Name)] = true
	}
	if output == nil || output.Kind != "extend" || len(packages) != len(resolved.PackageInputs) {
		return false
	}
	for _, operation := range output.Operations {
		switch operation.Name {
		case "arg", "env", "label", "user", "cmd", "entrypoint", "shell", "stopsignal", "expose", "volume", "maintainer", "workdir":
			if len(operation.Children) != 0 {
				return false
			}
		case "run":
			if len(controls.Devices) != 0 || !componentCacheRunEligible(operation, packages) {
				return false
			}
		case "copy", "add":
			if len(operation.Children) != 0 || !packages[strings.ToLower(operation.Properties["from"])] {
				return false
			}
		default:
			return false
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
func (c *componentCache) lookup(ctx context.Context, executor *graphExecutor, key cache.Key, callerImageID string, callerConfig json.RawMessage, callerRoot *PackageRootMetadata, platform v1.Platform, system *types.SystemContext) (string, *imageconfig.Config, bool, error) {
	for _, store := range c.readStores {
		if err := ctx.Err(); err != nil {
			return "", nil, false, err
		}
		record, path, err := store.Lookup(ctx, key)
		if err := ctx.Err(); err != nil {
			return "", nil, false, err
		}
		if err != nil {
			if ctx.Err() != nil {
				return "", nil, false, ctx.Err()
			}
			if !errors.Is(err, cache.ErrMiss) {
				packageCacheWarning("read component cache", err)
				c.stats.Errors++
			}
			continue
		}
		if path != "" {
			defer func() { _ = os.Remove(path) }()
		}
		if record == nil {
			packageCacheWarning("read component cache", errors.New("cache returned an incomplete component record"))
			c.stats.Errors++
			continue
		}
		if !cache.RecordFresh(record.CreatedAt, c.cacheTTL, time.Now()) {
			continue
		}
		imageID := callerImageID
		snapshotConfig := callerConfig
		if record.ChangedFS {
			if record.Snapshot == nil || path == "" {
				packageCacheWarning("read component cache", errors.New("cache returned an incomplete component snapshot"))
				c.stats.Errors++
				continue
			}
			file, openErr := os.Open(path)
			if openErr != nil {
				packageCacheWarning("open component cache snapshot", openErr)
				c.stats.Errors++
				continue
			}
			claimed, identityErr := stateidentity.Calculate(ctx, callerRoot.tarHeader(), file, record.Config, nil)
			closeErr := file.Close()
			if err := ctx.Err(); err != nil {
				return "", nil, false, err
			}
			if verifyErr := errors.Join(identityErr, closeErr); verifyErr != nil || claimed != record.Output {
				if verifyErr == nil {
					verifyErr = errors.New("snapshot identity differs from cache record")
				}
				packageCacheWarning("verify component cache snapshot", verifyErr)
				c.stats.Errors++
				continue
			}
			imageID, snapshotConfig, err = ImportPackageSnapshot(ctx, executor.store, system, *record.Snapshot, path, platform)
			if cancelErr := ctx.Err(); cancelErr != nil {
				return "", nil, false, cancelErr
			}
			if err != nil {
				packageCacheWarning("import component cache snapshot", err)
				c.stats.Errors++
				continue
			}
		}
		config, err := imageconfig.Parse(record.Config)
		if err != nil {
			packageCacheWarning("parse component cache config", err)
			c.stats.Errors++
			continue
		}
		observed, _, snapshotPath, eligible, err := snapshotPortableState(ctx, executor.store, system, imageID, snapshotConfig, record.Config, callerRoot, platform, c.stagingDir)
		if snapshotPath != "" {
			_ = os.Remove(snapshotPath)
		}
		if cancelErr := ctx.Err(); cancelErr != nil {
			return "", nil, false, cancelErr
		}
		if err != nil || !eligible || observed != record.Output {
			if err == nil {
				err = errors.New("restored state does not match cache record")
			}
			packageCacheWarning("verify restored component cache", err)
			c.stats.Errors++
			continue
		}
		// A remote hit can seed earlier (typically local) cache stores so a
		// later offline build can reuse the same validated record.
		var seedErr error
		for _, earlier := range c.writeStores {
			if sameCacheStore(earlier, store) {
				continue
			}
			if err := ctx.Err(); err != nil {
				return "", nil, false, err
			}
			_, err := earlier.Put(ctx, key, *record, path)
			if cancelErr := ctx.Err(); cancelErr != nil {
				return "", nil, false, cancelErr
			}
			if err != nil {
				c.stats.Errors++
				seedErr = errors.Join(seedErr, fmt.Errorf("seed component cache: %w", err))
			} else {
				c.stats.Stored++
			}
		}
		if seedErr != nil {
			if path != "" {
				seedErr = errors.Join(seedErr, os.Remove(path))
			}
			return "", nil, false, seedErr
		}
		c.stats.Hits++
		return imageID, config, true, nil
	}
	if err := ctx.Err(); err != nil {
		return "", nil, false, err
	}
	c.stats.Misses++
	return "", nil, false, nil
}

func (c *componentCache) record(ctx context.Context, executor *graphExecutor, key cache.Key, outputImageID string, outputConfig *imageconfig.Config, root *PackageRootMetadata, platform v1.Platform, system *types.SystemContext) {
	if c == nil || len(c.writeStores) == 0 {
		return
	}
	raw, err := outputConfig.MarshalJSON()
	if err != nil {
		packageCacheWarning("prepare component cache candidate", err)
		c.stats.Errors++
		return
	}
	identity, pkg, path, eligible, err := snapshotPortableState(ctx, executor.store, system, outputImageID, raw, raw, root, platform, c.stagingDir)
	if err != nil || !eligible {
		packageCacheWarning("snapshot component cache candidate", err)
		c.stats.Skipped++
		return
	}
	changed := identity.Filesystem != key.Input.Filesystem
	record := cache.Record{Version: cache.RecordVersion, CreatedAt: time.Now().UTC(), Key: key, Output: identity, Config: raw, ChangedFS: changed}
	if changed {
		pkg.Descriptor.MediaType = cache.SnapshotMediaType
		record.Snapshot = &pkg
		importedID, importedConfig, importErr := ImportPackageSnapshot(ctx, executor.store, system, pkg, path, platform)
		if importErr != nil {
			packageCacheWarning("import component cache candidate", importErr)
			_ = os.Remove(path)
			c.stats.Skipped++
			return
		}
		roundtrip, _, roundtripPath, ok, observeErr := snapshotPortableState(ctx, executor.store, system, importedID, importedConfig, raw, root, platform, c.stagingDir)
		if roundtripPath != "" {
			_ = os.Remove(roundtripPath)
		}
		if observeErr != nil || !ok || roundtrip != identity {
			packageCacheWarning("verify component cache candidate", observeErr)
			_ = os.Remove(path)
			c.stats.Skipped++
			return
		}
	} else {
		_ = os.Remove(path)
		path = ""
	}
	c.candidates = append(c.candidates, componentCacheCandidate{key: key, record: record, path: path})
}

func (c *componentCache) publish(ctx context.Context) error {
	if c == nil {
		return ctx.Err()
	}
	var exportErr error
	for _, candidate := range c.candidates {
		for _, store := range c.writeStores {
			if err := ctx.Err(); err != nil {
				break
			}
			_, err := store.Put(ctx, candidate.key, candidate.record, candidate.path)
			if cancelErr := ctx.Err(); cancelErr != nil {
				break
			}
			if err != nil {
				c.stats.Errors++
				exportErr = errors.Join(exportErr, fmt.Errorf("write component cache: %w", err))
			} else {
				c.stats.Stored++
			}
		}
		if candidate.path != "" {
			if err := os.Remove(candidate.path); err != nil && !errors.Is(err, os.ErrNotExist) {
				exportErr = errors.Join(exportErr, fmt.Errorf("remove staged component cache: %w", err))
			}
		}
	}
	c.candidates = nil
	return errors.Join(ctx.Err(), exportErr)
}
