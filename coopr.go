// Package coopr executes typed build programs through Coopr's supervised native
// Buildah backend. Applications must call InitReexec at the start of main before
// parsing arguments. Workers reexecute that same binary and enter rootless user
// namespaces themselves; the host application need not enter a user namespace.
//
// Execution requires Linux, configured subordinate IDs/rootless namespaces,
// containers/storage support and an OCI runtime when running commands. Host or
// nested-container policy may prevent execution. Callers own store/cache roots,
// input contexts and successful output paths. Execute owns disposable workers and
// staging directories, and waits for cancellation cleanup before returning.
package coopr

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"

	"github.com/strio-dev/coopr/api"
	"github.com/strio-dev/coopr/internal/buildah"
	"github.com/strio-dev/coopr/internal/buildcontext"
	"golang.org/x/sys/unix"
)

// StateID identifies a state within one program.
type StateID string

// Program is an ordered, topological graph with one explicit final output.
// Platform defaults to the host platform. Independent states may run concurrently.
// Values are literal; coopr does not expand Coopr ARG or ENV expressions.
type Program struct {
	Platform string
	States   []State
	Output   StateID
}

// State derives a filesystem/image state from exactly one image or earlier state.
type State struct {
	ID         StateID
	From       Source
	Operations []Operation
}

// Source selects an image, earlier state or named local COPY context. Image "scratch"
// selects an empty filesystem. Inherited ONBUILD triggers are cleared without
// execution; frontend instruction expansion is outside this API. An empty Copy
// or bind-mount source uses ContextDir. Context selects a declared local named
// input for COPY or RUN mounts; it cannot supply a state base.
type Source struct {
	Context string
	Image   string
	State   StateID
}

// Operation is a supported typed build operation. Use value forms of Run, Copy,
// Env and Workdir; pointer forms are not accepted.
type Operation interface{ operation() }

// Run executes Args directly, without adding a shell. Use an explicit shell argv
// when shell interpretation is desired.
type Run struct {
	Args   []string
	Mounts []Mount
}

func (Run) operation() {}

// Copy copies Sources into Destination from an image, state or host context.
type Copy struct {
	Sources     []string
	Destination string
	From        Source
}

func (Copy) operation() {}

// Env sets one literal image environment value inherited by later commands.
type Env struct{ Name, Value string }

func (Env) operation() {}

// Workdir selects a literal working directory.
type Workdir struct{ Path string }

func (Workdir) operation() {}

// Mount is a bind, cache or tmpfs RUN mount. Omitted cache IDs use native stable
// program/platform/stage scope. Sharing is shared or locked; the current
// Buildah backend does not implement private sharing. From applies only to bind mounts.
// ReadOnly false requests writable binds. Writable host-context binds use the
// native disposable build-scoped context copy and side-effect-aware cache; the
// caller context is preserved. Other mount types retain native defaults when
// ReadOnly is false.
type Mount struct {
	// HostPath is an explicit live host directory for one bind RUN mount.
	// It is exclusive with From and Source. Live mounts bypass result caching;
	// ordinary context mounts retain their snapshotted semantics.
	HostPath string
	// Relabel explicitly opts a live host directory into native SELinux z/Z relabeling.
	Relabel                           string
	Type, Target, Source, ID, Sharing string
	From                              Source
	ReadOnly                          bool
}

// StoreOptions selects caller-owned containers/storage roots. Both roots must
// be absolute and distinct. Driver defaults to native containers/storage policy.
type StoreOptions struct {
	RunRoot, GraphRoot, Driver string
	DriverOptions              []string
}

// Output selects fresh absolute destinations whose parent directories exist.
// OCI is required. Filesystem optionally exports the flattened root filesystem.
// Each output is promoted after successful execution; promotion of two outputs
// is not a single filesystem transaction. Existing paths are never overwritten.
type Output struct{ OCI, Filesystem string }

// CacheOptions controls the optional portable instruction cache. LocalDir is a
// caller-owned absolute OCI cache directory. RegistryFrom and RegistryTo select
// bare registry repositories for reads and writes independently; they may be
// combined with LocalDir. Leaving all locations empty disables portable caching.
// Native local Buildah layer reuse is separate and is not counted by CacheStats.
type CacheOptions struct {
	// MountDir selects the private persistent parent for native RUN cache mounts.
	// Contents are disposable acceleration and are not hashed into build inputs.
	MountDir     string
	LocalDir     string
	NoCache      bool
	RegistryFrom []string
	RegistryTo   []string
}

// RegistryOptions controls native image authentication and pull policy.
type RegistryOptions struct {
	AuthFile, CertDir, Credentials, PullPolicy, SignaturePolicyPath string
	TLSVerify                                                       *bool
}

// Options supplies execution resources. ContextDir, when supplied, is an
// absolute directory. Jobs zero uses the backend default. Writers receive worker
// output; callers sharing writers across concurrent calls must synchronize them.
type Options struct {
	// Progress receives bounded observations after supervised workers complete.
	// Callers sharing a callback across concurrent Execute calls synchronize it.
	Progress func(OperationProgress)
	// Network selects the default RUN network; it does not restrict registry traffic.
	Network string
	// Contexts supplies named local COPY and RUN inputs independently of ContextDir.
	Contexts                       map[string]string
	Store                          StoreOptions
	ContextDir, Runtime, Isolation string
	Jobs                           int
	Output                         Output
	Cache                          CacheOptions
	Registry                       RegistryOptions
	Stdout, Stderr                 io.Writer
}

// CacheStats counts optional portable-cache decisions, not total backend reuse.
type CacheStats struct{ Hits, Misses, Stored, Skipped, Errors int }

// Result identifies the completed image and caller-owned outputs.
type Result struct {
	ImageID, ManifestDigest, Platform, OCI, Filesystem string
	Cache                                              CacheStats
	ProgressDropped                                    uint64
}

// OperationProgress is a backend-independent observation of execution work.
type OperationProgress = api.OperationProgress

var initialized atomic.Bool

// InitReexec dispatches registered same-binary build workers and runtime helpers.
// Call first in main, and return immediately when it returns true.
//
// Dispatch runs before application argument checks, following Buildah's
// embedding contract. It does not authenticate application requests. Do not
// expose the executable through setuid, file capabilities or restricted sudo
// rules that rely on those later checks to limit the caller's operations.
// Operator-authorized root and sudo builds use the operator's existing authority.
func InitReexec() bool {
	if buildah.InitReexec() {
		return true
	}
	initialized.Store(true)
	return false
}

// Execute validates and executes a static program using the same supervised
// graph scheduler, cache, process groups and cleanup path as Coopr's frontend.
func Execute(ctx context.Context, program Program, options Options) (result Result, retErr error) {
	if ctx == nil {
		return Result{}, errors.New("execution context is nil")
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if !initialized.Load() {
		return Result{}, errors.New("coopr.InitReexec must be called at the start of main before Execute")
	}
	network, _, err := buildah.NormalizeBuildNetworkOptions(options.Network, nil)
	if err != nil {
		return Result{}, fmt.Errorf("RUN network: %w", err)
	}
	options.Network = network
	cacheFrom, err := buildah.RegistryCacheSpecs(options.Cache.RegistryFrom)
	if err != nil {
		return Result{}, fmt.Errorf("registry cache sources: %w", err)
	}
	cacheTo, err := buildah.RegistryCacheSpecs(options.Cache.RegistryTo)
	if err != nil {
		return Result{}, fmt.Errorf("registry cache destinations: %w", err)
	}
	options.Store.DriverOptions = slices.Clone(options.Store.DriverOptions)
	if options.Registry.TLSVerify != nil {
		value := *options.Registry.TLSVerify
		options.Registry.TLSVerify = &value
	}
	if options.Cache.MountDir != "" {
		cacheRoot, err := canonicalResource(options.Cache.MountDir)
		if err != nil {
			return Result{}, fmt.Errorf("mount cache directory: %w", err)
		}
		for _, resource := range []string{options.ContextDir, options.Store.RunRoot, options.Store.GraphRoot, options.Cache.LocalDir, options.Output.OCI, options.Output.Filesystem} {
			if resource == "" {
				continue
			}
			normalized, err := canonicalResource(resource)
			if err != nil {
				return Result{}, err
			}
			if pathsOverlap(cacheRoot, normalized) {
				return Result{}, errors.New("mount cache directory overlaps context, store, result cache or output")
			}
		}
		options.Cache.MountDir = cacheRoot
	}
	contexts, err := localContextSpecs(options.Contexts)
	if err != nil {
		return Result{}, err
	}
	if err := validateContextResources(contexts, options); err != nil {
		return Result{}, err
	}
	for _, state := range program.States {
		for _, operation := range state.Operations {
			if run, ok := operation.(Run); ok {
				for _, mount := range run.Mounts {
					if mount.HostPath == "" {
						continue
					}
					host, err := canonicalResource(mount.HostPath)
					if err != nil {
						return Result{}, fmt.Errorf("live host bind: %w", err)
					}
					for _, resource := range []string{options.Store.RunRoot, options.Store.GraphRoot, options.Cache.LocalDir, options.Cache.MountDir, options.Output.OCI, options.Output.Filesystem} {
						if resource == "" {
							continue
						}
						normalized, err := canonicalResource(resource)
						if err != nil {
							return Result{}, err
						}
						if pathsOverlap(host, normalized) {
							return Result{}, errors.New("live host bind overlaps store, cache or output")
						}
					}
				}
			}
		}
	}
	plan, err := compile(program, contexts...)
	if err != nil {
		return Result{}, err
	}
	if options.Jobs < 0 {
		return Result{}, errors.New("jobs must not be negative")
	}
	for _, item := range []struct {
		name, path string
		required   bool
	}{{"OCI output", options.Output.OCI, true}, {"filesystem output", options.Output.Filesystem, false}} {
		if item.path == "" && !item.required {
			continue
		}
		if err := freshOutput(item.path); err != nil {
			return Result{}, fmt.Errorf("%s: %w", item.name, err)
		}
	}
	if options.Cache.LocalDir != "" && !filepath.IsAbs(options.Cache.LocalDir) {
		return Result{}, errors.New("cache directory must be absolute")
	}
	if options.Output.Filesystem != "" && pathsOverlap(options.Output.OCI, options.Output.Filesystem) {
		return Result{}, errors.New("output destinations must not overlap")
	}
	// A live host input is deliberately outside immutable build snapshots.
	// Conservatively bypass every result cache in this invocation so neither a
	// writable effect nor a changing read-only live view can be skipped.
	for _, state := range program.States {
		for _, operation := range state.Operations {
			if run, ok := operation.(Run); ok {
				for _, mount := range run.Mounts {
					if mount.HostPath != "" {
						options.Cache.NoCache = true
					}
				}
			}
		}
	}
	controls, err := buildah.ParseRunControls(buildah.RunControlInput{
		HTTPProxy: true, Runtime: options.Runtime, Isolation: options.Isolation,
	})
	if err != nil {
		return Result{}, fmt.Errorf("native execution defaults: %w", err)
	}
	out := buildah.Output{Path: options.Output.OCI}
	var contextArtifacts []string
	if options.Output.Filesystem != "" {
		staging, err := os.MkdirTemp(filepath.Dir(options.Output.Filesystem), ".coopr-export-*")
		if err != nil {
			return Result{}, err
		}
		defer func() {
			retErr = buildah.RemoveWorkerDirectory(staging, retErr)
		}()
		out.Filesystem = buildah.FilesystemOutput{Type: "local", Path: filepath.Join(staging, "rootfs")}
		contextArtifacts = []string{staging}
	}
	native, err := buildah.BuildPlanSupervised(ctx, plan, buildah.SupervisedPlanOptions{
		Store:      buildah.StoreOptions{RunRoot: options.Store.RunRoot, GraphRoot: options.Store.GraphRoot, GraphDriverName: options.Store.Driver, GraphDriverOptions: options.Store.DriverOptions},
		ContextDir: options.ContextDir, ContextArtifacts: contextArtifacts, Runtime: options.Runtime, Isolation: options.Isolation, Jobs: options.Jobs, Output: out,
		Network:       options.Network,
		RunControls:   controls,
		CacheMountDir: options.Cache.MountDir, CacheLocalDir: options.Cache.LocalDir, CacheFrom: cacheFrom, CacheTo: cacheTo, NoCache: options.Cache.NoCache,
		AuthFile: options.Registry.AuthFile, CertDir: options.Registry.CertDir, Credentials: options.Registry.Credentials, TLSVerify: options.Registry.TLSVerify, PullPolicy: options.Registry.PullPolicy, SignaturePolicyPath: options.Registry.SignaturePolicyPath,
		Stdout: options.Stdout, Stderr: options.Stderr, Progress: options.Progress,
	})
	if err != nil {
		return Result{ProgressDropped: native.ProgressDropped}, err
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if options.Output.Filesystem != "" {
		if err := promoteFilesystem(out.Filesystem.Path, options.Output.Filesystem); err != nil {
			return Result{}, fmt.Errorf("promote filesystem output: %w", err)
		}
	}
	return Result{ImageID: native.ImageID, ManifestDigest: native.ManifestDigest, Platform: native.Platform, OCI: native.Layout, Filesystem: options.Output.Filesystem, ProgressDropped: native.ProgressDropped, Cache: CacheStats{Hits: native.CacheStats.Hits, Misses: native.CacheStats.Misses, Stored: native.CacheStats.Stored, Skipped: native.CacheStats.Skipped, Errors: native.CacheStats.Errors}}, nil
}
func freshOutput(path string) error {
	if !filepath.IsAbs(path) {
		return errors.New("destination must be absolute")
	}
	if _, err := os.Lstat(path); err == nil {
		return errors.New("destination already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	info, err := os.Stat(filepath.Dir(path))
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("destination parent is not a directory")
	}
	return nil
}
func pathsOverlap(a, b string) bool {
	within := func(parent, child string) bool {
		rel, err := filepath.Rel(parent, child)
		return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
	}
	return within(a, b) || within(b, a)
}

// Match the backend's output promotion: a concurrent creator retains its path.
func promoteFilesystem(source, destination string) error {
	return unix.Renameat2(unix.AT_FDCWD, source, unix.AT_FDCWD, destination, unix.RENAME_NOREPLACE)
}

// localContextSpecs snapshots the names and canonical directories before workers
// start. Named input selection is explicit and never inferred from an image name.
func localContextSpecs(contexts map[string]string) ([]buildcontext.Spec, error) {
	names := make([]string, 0, len(contexts))
	for name := range contexts {
		names = append(names, name)
	}
	slices.Sort(names)
	specs := make([]buildcontext.Spec, 0, len(names))
	for _, name := range names {
		directory := contexts[name]
		if !filepath.IsAbs(directory) {
			return nil, fmt.Errorf("named context %q directory must be absolute", name)
		}
		parsed, err := buildcontext.Parse([]string{name + "=" + directory})
		if err != nil {
			return nil, err
		}
		spec := parsed[0]
		if spec.Name != name || name == "build" || spec.Kind != buildcontext.Local {
			return nil, fmt.Errorf("invalid or reserved local context name %q", name)
		}
		spec.Path, err = filepath.EvalSymlinks(directory)
		if err != nil {
			return nil, fmt.Errorf("named context %q: %w", name, err)
		}
		info, err := os.Stat(spec.Path)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("named context %q must be a directory", name)
		}
		specs = append(specs, spec)
	}
	return specs, nil
}

func validateContextResources(contexts []buildcontext.Spec, options Options) error {
	if len(contexts) == 0 {
		return nil
	}
	for _, resource := range []string{options.Store.RunRoot, options.Store.GraphRoot, options.Cache.LocalDir, options.Cache.MountDir, options.Output.OCI, options.Output.Filesystem} {
		if resource == "" {
			continue
		}
		normalized, err := canonicalResource(resource)
		if err != nil {
			return err
		}
		for _, input := range contexts {
			if pathsOverlap(input.Path, normalized) {
				return fmt.Errorf("named context %q overlaps store, cache or output", input.Name)
			}
		}
	}
	return nil
}

func canonicalResource(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", errors.New("execution resource must be absolute")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err == nil {
		return resolved, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	parent := filepath.Dir(path)
	if parent == path {
		return "", err
	}
	resolved, err = canonicalResource(parent)
	if err != nil {
		return "", err
	}
	return filepath.Join(resolved, filepath.Base(path)), nil
}
