package buildah

import (
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

	"coopr/internal/cache"
	"coopr/internal/oci"
	"coopr/internal/planner"
	"github.com/containerd/platforms"
	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	upstream "go.podman.io/buildah"
	"go.podman.io/buildah/copier"
	"go.podman.io/buildah/define"
	"go.podman.io/buildah/pkg/parse"
	"go.podman.io/storage"
)

const packageCacheLoweringVersion = "coopr-buildah-package-v1"

var errPackageCacheDriverUnknown = errors.New("package cache storage driver is unknown")
var errPackageContextUnselected = errors.New("named context was not selected during planning")

type packageResultCache struct {
	readStores  []cache.PackageStore
	writeStores []cache.PackageStore
	stagingDir  string
	cacheTTL    *time.Duration
}

type packageCacheHit struct {
	record cache.PackageRecord
	path   string
}

func newPackageResultCache(ctx context.Context, options PlanOptions) (*packageResultCache, error) {
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
	dir, err := os.MkdirTemp("", "coopr-package-cache-*")
	if err != nil {
		return nil, fmt.Errorf("create package cache staging: %w", err)
	}
	result := &packageResultCache{stagingDir: dir, cacheTTL: options.CacheTTL}
	for _, binding := range bindings {
		var store cache.PackageStore
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
			if binding.write {
				return nil, errors.Join(fmt.Errorf("open package cache %s:%s: %w", binding.spec.Transport, binding.spec.Reference, err), os.RemoveAll(dir))
			}
			action := "open registry package cache"
			if binding.spec.Transport == "oci-layout" {
				action = "open local package cache"
			}
			packageCacheWarning(action, err)
			continue
		}
		if binding.read {
			result.readStores = append(result.readStores, store)
		}
		if binding.write {
			result.writeStores = append(result.writeStores, store)
		}
	}
	if len(result.readStores) == 0 && len(result.writeStores) == 0 {
		_ = os.RemoveAll(dir)
		return nil, nil
	}
	return result, nil
}

func packageCacheWarning(action string, err error) {
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "coopr: warning: %s: %v\n", action, err)
	}
}

func (c *packageResultCache) close() error {
	if c == nil {
		return nil
	}
	return os.RemoveAll(c.stagingDir)
}

// packageCacheEligibility follows conventional container-build memoization.
// Network responses, credential contents, and cache-mount contents are session
// inputs and deliberately do not invalidate an otherwise identical package
// result. Local bind/COPY/ADD inputs are represented by the filtered context
// digest, while stage, image, and named-context inputs are immutable selected
// descriptors. Host/device execution remains excluded because package lookup
// happens before executor entitlement and CDI resolution.
func packageCacheEligibility(stages []planner.Stage, controls RunControls) (needsContext bool, eligible bool) {
	for _, stage := range stages {
		for _, operation := range stage.Operations {
			switch operation.Name {
			case "run":
				if network := operation.Properties["network"]; network != "" && network != "default" && network != "none" {
					return false, false
				}
				if len(controls.Devices) != 0 || operation.Properties["security"] == "insecure" || hasRunDeviceOptions(operation) {
					return false, false
				}
				for _, child := range operation.Children {
					if child.Name != "mount" || len(child.Arguments) != 1 {
						return false, false
					}
					switch child.Arguments[0] {
					case "bind":
						if child.Properties["from"] == "" {
							needsContext = true
						}
					case "cache", "tmpfs", "secret", "ssh":
					default:
						return false, false
					}
				}
			case "copy", "add":
				if len(operation.Arguments) > 1 && operation.Properties["from"] == "" {
					needsContext = true
				}
				if operation.Name == "add" {
					last := len(operation.Arguments) - 1
					if last < 0 {
						last = 0
					}
					for _, source := range operation.Arguments[:last] {
						if (isHTTPAddSource(source) || isGitAddSource(source)) && operation.Properties["checksum"] == "" {
							return false, false
						}
					}
				}
			case "component":
				return false, false
			}
		}
	}
	return needsContext, true
}

func packageOutputClosure(stages []planner.Stage, output string) ([]planner.Stage, error) {
	byID := make(map[string]planner.Stage, len(stages))
	for _, stage := range stages {
		byID[stage.ID] = stage
	}
	if _, ok := byID[output]; !ok {
		return nil, fmt.Errorf("unknown package output stage %q", output)
	}
	wanted := make(map[string]bool)
	var visit func(string) error
	visit = func(id string) error {
		if wanted[id] {
			return nil
		}
		stage, ok := byID[id]
		if !ok {
			return fmt.Errorf("package stage %q depends on missing stage %q", output, id)
		}
		wanted[id] = true
		for _, dependency := range stage.Dependencies {
			if err := visit(dependency); err != nil {
				return err
			}
		}
		return nil
	}
	if err := visit(output); err != nil {
		return nil, err
	}
	closure := make([]planner.Stage, 0, len(wanted))
	for _, stage := range stages {
		if wanted[stage.ID] {
			closure = append(closure, stage)
		}
	}
	return closure, nil
}

func packagePlanDigest(plan *planner.Plan, closure []planner.Stage, options PlanOptions) (digest.Digest, error) {
	document := struct {
		DefinitionType   string            `json:"definition_type"`
		Platform         string            `json:"platform"`
		Stages           []planner.Stage   `json:"stages"`
		PackageArguments map[string]string `json:"package_arguments,omitempty"`
		SourceDateEpoch  *int64            `json:"source_date_epoch,omitempty"`
		Timestamp        *int64            `json:"timestamp,omitempty"`
		RewriteTimestamp bool              `json:"rewrite_timestamp,omitempty"`
	}{plan.DefinitionType, plan.Platform, closure, plan.PackageArguments, options.SourceDateEpoch, options.Timestamp, options.RewriteTimestamp}
	data, err := json.Marshal(document)
	if err != nil {
		return "", err
	}
	return digest.FromBytes(data), nil
}

func resolvePackageClosureBases(closure []planner.Stage, selected map[ResolvedBaseKey]ResolvedImageSource) (map[string]v1.Descriptor, error) {
	aliases := make(map[string]bool, len(closure))
	bases := make(map[string]v1.Descriptor)
	resolve := func(reference, platformString string) error {
		if reference == "" || strings.EqualFold(reference, "scratch") || aliases[strings.ToLower(reference)] {
			return nil
		}
		if _, err := platforms.Parse(platformString); err != nil {
			return err
		}
		selection, ok := selected[ResolvedBaseKey{Reference: reference, Platform: platformString}]
		if !ok || selection.Selected.Digest == "" {
			return fmt.Errorf("external image %q for %s was not selected during planning", reference, platformString)
		}
		descriptor := v1.Descriptor{MediaType: selection.Selected.MediaType, Digest: selection.Selected.Digest, Size: selection.Selected.Size}
		identity := digest.FromString(reference + "\x00" + platformString).Encoded()
		bases["image-"+identity] = descriptor
		return nil
	}
	resolveContext := func(name, platformString string) error {
		if name == "" {
			return nil
		}
		if _, err := platforms.Parse(platformString); err != nil {
			return err
		}
		selection, ok := selected[graphNamedContextKey(name, platformString)]
		if !ok || selection.Selected.Digest == "" {
			return fmt.Errorf("%w: %q for %s", errPackageContextUnselected, name, platformString)
		}
		descriptor := v1.Descriptor{MediaType: selection.Selected.MediaType, Digest: selection.Selected.Digest, Size: selection.Selected.Size}
		identity := digest.FromString(strings.ToLower(name) + "\x00" + platformString).Encoded()
		bases["context-"+identity] = descriptor
		return nil
	}
	for _, stage := range closure {
		if stage.SourceContext != "" {
			if err := resolveContext(stage.SourceContext, stage.Platform); err != nil {
				return nil, fmt.Errorf("resolve package stage %s base: %w", stage.ID, err)
			}
		} else if err := resolve(stage.Source, stage.Platform); err != nil {
			return nil, fmt.Errorf("resolve package stage %s base: %w", stage.ID, err)
		}
		for index, operation := range stage.Operations {
			for _, reference := range operationStageReferences(operation) {
				if reference.context != "" {
					if err := resolveContext(reference.context, stage.Platform); err != nil {
						return nil, fmt.Errorf("resolve package stage %s operation %d context: %w", stage.ID, index+1, err)
					}
				} else if err := resolve(reference.source, stage.Platform); err != nil {
					return nil, fmt.Errorf("resolve package stage %s operation %d image: %w", stage.ID, index+1, err)
				}
			}
		}
		aliases[stage.ID] = true
		if stage.Name != "" {
			aliases[strings.ToLower(stage.Name)] = true
		}
	}
	return bases, nil
}

func packageContextIdentity(ctx context.Context, contextDir string, artifacts []string, ignoreFile string) (digest.Digest, error) {
	policy, err := prepareContextPolicyWithIgnore(contextDir, artifacts, ignoreFile)
	if err != nil {
		return "", err
	}
	options, err := policy.apply(upstream.AddAndCopyOptions{})
	if err != nil {
		return "", err
	}
	snapshot, cleanup, err := snapshotContext(options.ContextDir, options.Excludes, nil, nil)
	if err != nil {
		return "", err
	}
	defer func() { _ = cleanup() }()
	digester := digest.Canonical.Digester()
	if err := copier.Get(snapshot, snapshot, copier.GetOptions{}, []string{"."}, cacheContextWriter{ctx: ctx, writer: digester.Hash()}); err != nil {
		return "", fmt.Errorf("hash filtered package context: %w", err)
	}
	return digester.Digest(), nil
}

func packageCacheExecutor(options PlanOptions, runtimeIdentity string) (string, error) {
	modules, err := cacheModuleVersions()
	if err != nil {
		return "", err
	}
	isolation, err := parse.IsolationOption(options.Isolation)
	if err != nil {
		return "", err
	}
	driver := options.Store.GraphDriverName
	if driver == "" {
		defaults, err := storage.DefaultStoreOptions()
		if err != nil {
			return "", fmt.Errorf("resolve default storage driver: %w", err)
		}
		driver = defaults.GraphDriverName
	}
	if driver == "" {
		return "", errPackageCacheDriverUnknown
	}
	base, err := componentCacheExecutorSemantics(define.Version, modules, isolation.String(), driver, options.Store.GraphDriverOptions, runtimeIdentity, outputFormatDocker)
	if err != nil {
		return "", err
	}
	executionOptions, err := buildAddHostsDigest(options.AddHosts)
	if err != nil {
		return "", err
	}
	runControls, err := cacheRunControlsDigest(options.RunControls)
	if err != nil {
		return "", err
	}
	return "buildah-" + digest.FromString(base+"\x00"+executionOptions.String()+"\x00"+runControls.String()).Encoded(), nil
}

func packageCacheKeys(ctx context.Context, options PlanOptions, plan *planner.Plan, stages []planner.Stage, outputs map[string]packageOutput, artifacts []string) (map[string]cache.PackageKey, bool, error) {
	if !buildResultCacheEligible(options.AddHosts) {
		return nil, false, nil
	}
	needsContext, eligible := packageCacheEligibility(stages, options.RunControls)
	runtimeIdentity, runtimeErr := instructionRuntimeIdentity(effectiveRuntime(options.Runtime, options.RunControls))
	if !eligible || runtimeErr != nil && packageStagesNeedRuntime(stages) {
		return nil, false, nil
	}
	var contextIdentity digest.Digest
	var err error
	if needsContext {
		contextIdentity, err = packageContextIdentity(ctx, options.ContextDir, artifacts, options.IgnoreFile)
		if err != nil {
			return nil, false, err
		}
	}
	executorIdentity, err := packageCacheExecutor(options, runtimeIdentity)
	if err != nil {
		if errors.Is(err, errPackageCacheDriverUnknown) {
			return nil, false, nil
		}
		return nil, false, nil
	}
	platform, err := platforms.Parse(plan.Platform)
	if err != nil {
		return nil, false, err
	}
	keys := make(map[string]cache.PackageKey, len(outputs))
	for stageID, output := range outputs {
		closure, err := packageOutputClosure(stages, stageID)
		if err != nil {
			return nil, false, err
		}
		planDigest, err := packagePlanDigest(plan, closure, options)
		if err != nil {
			return nil, false, err
		}
		bases, err := resolvePackageClosureBases(closure, options.ResolvedBases)
		if err != nil {
			if errors.Is(err, errPackageContextUnselected) {
				return nil, false, nil
			}
			return nil, false, err
		}
		key := cache.PackageKey{
			Plan: planDigest, Output: output.key, Bases: bases, Context: contextIdentity,
			Platform: platform, Executor: executorIdentity,
			Frontend: "coopr-definition-v1", Lowering: packageCacheLoweringVersion,
		}
		if _, err := key.Digest(); err != nil {
			return nil, false, err
		}
		keys[stageID] = key
	}
	return keys, true, nil
}

func packageStagesNeedRuntime(stages []planner.Stage) bool {
	for _, stage := range stages {
		for _, operation := range stage.Operations {
			if operation.Name == "run" {
				return true
			}
		}
	}
	return false
}

func (c *packageResultCache) lookupAll(ctx context.Context, keys map[string]cache.PackageKey) (map[string]packageCacheHit, bool, error) {
	hits := make(map[string]packageCacheHit, len(keys))
	stageIDs := make([]string, 0, len(keys))
	for stageID := range keys {
		stageIDs = append(stageIDs, stageID)
	}
	slices.Sort(stageIDs)
	for _, stageID := range stageIDs {
		key := keys[stageID]
		var hit packageCacheHit
		found := false
		for _, store := range c.readStores {
			record, path, err := store.LookupPackage(ctx, key)
			if err := ctx.Err(); err != nil {
				return nil, false, err
			}
			if errors.Is(err, cache.ErrMiss) {
				continue
			}
			if err != nil {
				packageCacheWarning("read package cache", err)
				continue
			}
			if record == nil || path == "" {
				packageCacheWarning("read package cache", errors.New("cache returned an incomplete package record"))
				continue
			}
			if !cache.RecordFresh(record.CreatedAt, c.cacheTTL, time.Now()) {
				_ = os.Remove(path)
				continue
			}
			// Seed earlier stores only after the record and tar were verified by
			// the source store.
			var seedErr error
			for _, earlier := range c.writeStores {
				if sameCacheStore(earlier, store) {
					continue
				}
				if _, err := earlier.PutPackage(ctx, key, *record, path); err != nil {
					seedErr = errors.Join(seedErr, fmt.Errorf("seed package cache: %w", err))
				}
			}
			if seedErr != nil {
				_ = os.Remove(path)
				for _, retained := range hits {
					_ = os.Remove(retained.path)
				}
				return nil, false, seedErr
			}
			hit = packageCacheHit{record: *record, path: path}
			found = true
			break
		}
		if !found {
			for _, retained := range hits {
				_ = os.Remove(retained.path)
			}
			return nil, false, nil
		}
		hits[stageID] = hit
	}
	return hits, true, nil
}

func installCachedPackage(ctx context.Context, source, destination string, descriptor v1.Descriptor) error {
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(destination), ".coopr-package-cache-*")
	if err != nil {
		return err
	}
	path := temporary.Name()
	defer func() {
		_ = temporary.Close()
		_ = os.Remove(path)
	}()
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	digester := digest.Canonical.Digester()
	written, copyErr := io.Copy(io.MultiWriter(temporary, digester.Hash()), &contextReader{ctx: ctx, reader: input})
	closeErr := input.Close()
	if err := errors.Join(copyErr, closeErr); err != nil {
		return err
	}
	if written != descriptor.Size || digester.Digest() != descriptor.Digest {
		return errors.New("cached package changed while installing")
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(path, destination)
}

func installPackageCacheHits(ctx context.Context, hits map[string]packageCacheHit, outputs map[string]packageOutput) (map[string]oci.Package, error) {
	packages := make(map[string]oci.Package, len(hits))
	for stageID, hit := range hits {
		output := outputs[stageID]
		if err := installCachedPackage(ctx, hit.path, output.path, hit.record.Descriptor); err != nil {
			return nil, fmt.Errorf("install cached package %q: %w", output.key, err)
		}
		packages[output.key] = oci.Package{Stage: hit.record.Stage, Descriptor: hit.record.Descriptor, Config: slices.Clone(hit.record.Config)}
	}
	return packages, nil
}

func (c *packageResultCache) store(ctx context.Context, keys map[string]cache.PackageKey, outputs map[string]packageOutput, packages map[string]oci.Package) error {
	var exportErr error
	stageIDs := make([]string, 0, len(keys))
	for stageID := range keys {
		stageIDs = append(stageIDs, stageID)
	}
	slices.Sort(stageIDs)
	for _, stageID := range stageIDs {
		key := keys[stageID]
		output := outputs[stageID]
		pkg, ok := packages[output.key]
		if !ok {
			continue
		}
		record := cache.PackageRecord{Version: cache.PackageRecordVersion, CreatedAt: time.Now().UTC(), Key: key, Stage: pkg.Stage, Descriptor: pkg.Descriptor, Config: slices.Clone(pkg.Config)}
		for _, store := range c.writeStores {
			if ctx.Err() != nil {
				break
			}
			if _, err := store.PutPackage(ctx, key, record, output.path); err != nil {
				if ctx.Err() != nil {
					break
				}
				exportErr = errors.Join(exportErr, fmt.Errorf("write package cache: %w", err))
			}
		}
	}
	return errors.Join(ctx.Err(), exportErr)
}
