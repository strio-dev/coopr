// Package apibackend adapts public API requests to Coopr's existing backends.
package apibackend

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"

	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/coopr"
	"github.com/strio-dev/coopr/api"
	"github.com/strio-dev/coopr/internal/build"
	"github.com/strio-dev/coopr/internal/buildah"
	"github.com/strio-dev/coopr/internal/buildcontext"
	"github.com/strio-dev/coopr/internal/componentstore"
	"github.com/strio-dev/coopr/internal/oci"
	"github.com/strio-dev/coopr/internal/transfer"
	"golang.org/x/sys/unix"
)

func invalid(err error) error { return &api.Error{Code: "invalid_request", Message: err.Error()} }
func executionError(err error) error {
	if err == nil {
		return nil
	}
	code := "execution_failed"
	if errors.Is(err, buildah.ErrWorkerWaitIncomplete) {
		code = "cleanup_failed"
	} else if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		code = "canceled"
	}
	return &api.Error{Code: code, Message: err.Error()}
}

// Execute retains native literal/program and definition/component semantics.
func Execute(ctx context.Context, request api.Request, stdout, stderr io.Writer) (api.Result, error) {
	if ctx == nil {
		return api.Result{}, invalid(errors.New("context is nil"))
	}
	if err := ctx.Err(); err != nil {
		return api.Result{}, executionError(err)
	}
	if err := validate(request); err != nil {
		return api.Result{}, invalid(err)
	}
	if request.Kind == "component-load" {
		resolver, err := oci.NewResolver(oci.Options{ComponentStoreDir: request.ComponentLoad.ComponentStoreDir})
		if err != nil {
			return api.Result{}, executionError(err)
		}
		root, err := resolver.LoadComponent(ctx, request.ComponentLoad.Archive, request.ComponentLoad.Name)
		if err != nil {
			return api.Result{}, executionError(err)
		}
		reference := root.Digest.String()
		if request.ComponentLoad.Name != "" {
			name, _ := componentstore.NormalizeTag(request.ComponentLoad.Name) // Validated before loading.
			reference = name + "@" + root.Digest.String()
		}
		return api.Result{Reference: reference, ManifestDigest: root.Digest.String()}, nil
	}
	opts := request.Options
	cacheFrom, err := buildah.RegistryCacheSpecs(opts.Cache.RegistryFrom)
	if err != nil {
		return api.Result{}, invalid(err)
	}
	cacheTo, err := buildah.RegistryCacheSpecs(opts.Cache.RegistryTo)
	if err != nil {
		return api.Result{}, invalid(err)
	}
	namedContexts, err := localContexts(opts.Contexts)
	if err != nil {
		return api.Result{}, invalid(err)
	}
	store := buildah.StoreOptions{RunRoot: opts.Store.RunRoot, GraphRoot: opts.Store.GraphRoot, GraphDriverName: opts.Store.Driver, GraphDriverOptions: opts.Store.DriverOptions}
	if request.Kind == "execute" {
		program, err := program(*request.Program)
		if err != nil {
			return api.Result{}, invalid(err)
		}
		result, err := coopr.Execute(ctx, program, coopr.Options{Contexts: opts.Contexts, Store: coopr.StoreOptions(opts.Store), ContextDir: opts.ContextDir, Runtime: opts.Runtime, Isolation: opts.Isolation, Network: opts.Network, Jobs: opts.Jobs, Output: coopr.Output(opts.Output), Cache: coopr.CacheOptions(opts.Cache), Registry: coopr.RegistryOptions{AuthFile: opts.Registry.AuthFile, CertDir: opts.Registry.CertDir, PullPolicy: opts.Registry.PullPolicy, SignaturePolicyPath: opts.Registry.SignaturePolicyPath, TLSVerify: opts.Registry.TLSVerify}, Stdout: stdout, Stderr: stderr, Progress: api.ProgressObserver(ctx)})
		stats := api.CacheStats(result.Cache)
		return api.Result{ImageID: result.ImageID, ManifestDigest: result.ManifestDigest, Platform: result.Platform, OCI: result.OCI, Filesystem: result.Filesystem, Cache: &stats, ProgressDropped: result.ProgressDropped, Exports: result.Exports}, executionError(err)
	}
	if request.Kind == "publish" {
		_, finish := buildah.ObserveOperationPhase(ctx, buildah.OperationProgress{Instruction: "publish", Phase: "export"})
		result, err := publish(ctx, request, store, stderr)
		finish("", err)
		return result, err
	}
	controls, err := buildah.ParseRunControls(buildah.RunControlInput{HTTPProxy: true, Runtime: opts.Runtime, Isolation: opts.Isolation})
	if err != nil {
		return api.Result{}, invalid(err)
	}
	if err := os.MkdirAll(filepath.Dir(opts.Store.RunRoot), 0700); err != nil {
		return api.Result{}, executionError(err)
	}
	temporary, err := os.MkdirTemp(filepath.Dir(opts.Store.RunRoot), ".coopr-api-")
	if err != nil {
		return api.Result{}, executionError(err)
	}
	defer func() { _ = os.RemoveAll(temporary) }()
	metadata := filepath.Join(temporary, "result.json")
	var dropped atomic.Uint64
	recordDropped := func(count uint64) {
		for {
			previous := dropped.Load()
			next := previous + count
			if next < previous {
				next = ^uint64(0)
			}
			if dropped.CompareAndSwap(previous, next) {
				return
			}
		}
	}
	definition := request.Build
	requestedPlatform, err := normalizeBuildPlatform(definition.Platform)
	if err != nil {
		return api.Result{}, invalid(err)
	}
	result := api.Result{}
	if request.Kind == "build" {
		runBuild := func(ctx context.Context, target transfer.Destination) (string, error) {
			var outputs []buildah.FilesystemOutput
			if target.Name != "" {
				outputs = []buildah.FilesystemOutput{{Type: "local", Path: target.Name}}
			}
			built, buildErr := build.Run(ctx, build.Options{Platform: requestedPlatform, Files: definition.Files, File: definition.Files[0], Context: opts.ContextDir, BuildContexts: namedContexts, Target: definition.Target, Args: definition.Args, MetadataFile: metadata, Outputs: outputs, Exports: opts.Output.Exports, ExportResults: func(exports []api.ExportResult) { result.Exports = exports }, BuildStore: store, ComponentStoreDir: componentStore(definition.ComponentStoreDir, opts.Store.GraphRoot), CacheMountDir: opts.Cache.MountDir, CacheLocalDir: opts.Cache.LocalDir, CacheFrom: cacheFrom, CacheTo: cacheTo, NoCache: opts.Cache.NoCache, Network: opts.Network, RunControls: controls, Jobs: opts.Jobs, AuthFile: opts.Registry.AuthFile, CertDir: opts.Registry.CertDir, PullPolicy: opts.Registry.PullPolicy, SignaturePolicyPath: opts.Registry.SignaturePolicyPath, TLSVerify: opts.Registry.TLSVerify, Stdout: stdout, Stderr: stderr, Progress: buildah.OperationProgressFunc(api.ProgressObserver(ctx)), ProgressDropped: recordDropped})
			result.ImageID = built.ImageID
			return "", buildErr
		}
		if opts.Output.Filesystem == "" {
			_, err = runBuild(ctx, transfer.Destination{})
		} else {
			_, err = publishDestination(ctx, transfer.Destination{Transport: "dir", Name: opts.Output.Filesystem}, runBuild)
			if err == nil {
				result.Filesystem = opts.Output.Filesystem
			}
		}
	} else {
		ref, buildErr := build.BuildComponent(ctx, build.ComponentOptions{Platform: requestedPlatform, Files: definition.Files, File: definition.Files[0], Context: opts.ContextDir, BuildContexts: namedContexts, Target: definition.Target, Args: definition.Args, MetadataFile: metadata, StoreDir: componentStore(definition.ComponentStoreDir, opts.Store.GraphRoot), BuildStore: store, CacheMountDir: opts.Cache.MountDir, CacheLocalDir: opts.Cache.LocalDir, CacheFrom: cacheFrom, CacheTo: cacheTo, NoCache: opts.Cache.NoCache, Network: opts.Network, RunControls: controls, Jobs: opts.Jobs, AuthFile: opts.Registry.AuthFile, CertDir: opts.Registry.CertDir, PullPolicy: opts.Registry.PullPolicy, SignaturePolicyPath: opts.Registry.SignaturePolicyPath, TLSVerify: opts.Registry.TLSVerify, Stdout: stdout, Stderr: stderr, Progress: buildah.OperationProgressFunc(api.ProgressObserver(ctx)), ProgressDropped: recordDropped})
		result.Reference = ref
		err = buildErr
	}
	result.ProgressDropped = dropped.Load()
	data, readErr := os.ReadFile(metadata)
	if readErr == nil {
		if decodeErr := applyNativeResultMetadata(&result, data); decodeErr != nil {
			err = errors.Join(err, decodeErr)
		}
	} else if err == nil {
		err = readErr
	}

	if err == nil {
		_, finishExport := buildah.ObserveOperationPhase(ctx, buildah.OperationProgress{Instruction: request.Kind, Phase: "export"})
		kind := oci.Image
		source := result.ImageID
		destination := "oci:" + opts.Output.OCI
		if request.Kind == "component" {
			kind = oci.Component
			source = result.ManifestDigest
			result.Reference = source
			destination = "oci-archive:" + opts.Output.OCI
		}
		parsed, parseErr := transfer.ParsePushDestination(destination, kind)
		if parseErr != nil {
			err = parseErr
		} else {
			_, err = publishDestination(ctx, parsed, func(ctx context.Context, target transfer.Destination) (string, error) {
				return transfer.Copy(ctx, kind, source, target, transfer.Options{BuildStore: store, ComponentStoreDir: componentStore(definition.ComponentStoreDir, opts.Store.GraphRoot), AuthFile: opts.Registry.AuthFile, CertDir: opts.Registry.CertDir, TLSVerify: opts.Registry.TLSVerify, SignaturePolicyPath: opts.Registry.SignaturePolicyPath, ProgressWriter: stderr})
			})
		}
		if err == nil {
			result.OCI = opts.Output.OCI
			result.ManifestDigest, err = exportedDigest(opts.Output.OCI, request.Kind == "component")
		}
		finishExport("", err)
	}
	return result, executionError(platformExecutionError(err, requestedPlatform))
}

// The frontend's native manifest and an exported OCI manifest can differ after
// transport conversion. Identify the actual exported bytes; keep original
// frontend identities in Metadata rather than conflating the two.
func exportedDigest(path string, archive bool) (string, error) {
	indexPath := filepath.Join(path, "index.json")
	if archive {
		indexPath = path
	}
	file, err := os.Open(indexPath)
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()
	var reader io.Reader = file
	if archive {
		stream := tar.NewReader(file)
		for {
			header, e := stream.Next()
			if e != nil {
				return "", e
			}
			if strings.TrimPrefix(header.Name, "./") == "index.json" {
				reader = stream
				break
			}
		}
	}
	data, err := io.ReadAll(io.LimitReader(reader, api.MaxEventBytes+1))
	if err != nil {
		return "", err
	}
	if len(data) > api.MaxEventBytes {
		return "", errors.New("exported OCI index exceeds metadata limit")
	}
	var index v1.Index
	if err = json.Unmarshal(data, &index); err != nil {
		return "", err
	}
	if len(index.Manifests) != 1 {
		return "", errors.New("exported OCI layout must identify one root")
	}
	root := index.Manifests[0].Digest
	if err = root.Validate(); err != nil {
		return "", err
	}
	// Require an available digest algorithm instead of accepting an opaque token.
	if _, err = digest.Parse(root.String()); err != nil {
		return "", err
	}
	return root.String(), nil
}

func componentStore(path, graph string) string {
	if path != "" {
		return path
	}
	return graph + "-components"
}
func publish(ctx context.Context, r api.Request, store buildah.StoreOptions, out io.Writer) (api.Result, error) {
	p := r.Publish
	kind := oci.Image
	if p.Artifact == "component" {
		kind = oci.Component
	}
	result := api.Result{}
	var sourceDir string
	var sourceRoot v1.Descriptor
	if strings.HasPrefix(p.Source, "oci:") {
		var err error
		sourceDir, sourceRoot, err = publishOCISource(ctx, p)
		if err != nil {
			return result, executionError(err)
		}
	}
	var errs error
	for _, destination := range p.Destinations {
		if err := ctx.Err(); err != nil {
			return result, executionError(errors.Join(errs, err))
		}
		parsed, err := transfer.ParsePushDestination(destination, kind)
		ref := ""
		if err == nil {
			ref, err = publishDestination(ctx, parsed, func(ctx context.Context, target transfer.Destination) (string, error) {
				options := transfer.Options{BuildStore: store, ComponentStoreDir: componentStore(p.ComponentStoreDir, r.Options.Store.GraphRoot), AuthFile: r.Options.Registry.AuthFile, CertDir: r.Options.Registry.CertDir, TLSVerify: r.Options.Registry.TLSVerify, SignaturePolicyPath: r.Options.Registry.SignaturePolicyPath, ProgressWriter: out}
				if sourceDir != "" {
					return transfer.CopyRoot(ctx, kind, sourceDir, sourceRoot, target, options)
				}
				return transfer.Copy(ctx, kind, p.Source, target, options)
			})
		}
		item := api.Destination{Destination: destination, Reference: ref, Status: "passed"}
		if err != nil {
			item.Status = "failed"
			item.Error = err.Error()
			errs = errors.Join(errs, fmt.Errorf("destination %s: %w", destination, err))
		}
		result.Destinations = append(result.Destinations, item)
	}
	return result, executionError(errs)
}

func filesystemDestination(destination transfer.Destination) bool {
	switch destination.Transport {
	case "oci", "oci-archive", "dir", "docker-archive":
		return true
	default:
		return false
	}
}

// API filesystem publication owns staging only. A completed transfer is promoted
// atomically without replacing a destination created before or during copying.
func publishDestination(ctx context.Context, destination transfer.Destination, copyArtifact func(context.Context, transfer.Destination) (string, error)) (_ string, retErr error) {
	if !filesystemDestination(destination) {
		return copyArtifact(ctx, destination)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := fresh(destination.Name); err != nil {
		return "", err
	}
	final, err := canonical(destination.Name)
	if err != nil {
		return "", err
	}
	staging, err := os.MkdirTemp(filepath.Dir(final), ".coopr-publish-")
	if err != nil {
		return "", err
	}
	defer func() { retErr = buildah.RemoveWorkerDirectory(staging, retErr) }()
	target := destination
	target.Name = filepath.Join(staging, "artifact")
	reference, err := copyArtifact(ctx, target)
	if err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := unix.Renameat2(unix.AT_FDCWD, target.Name, unix.AT_FDCWD, final, unix.RENAME_NOREPLACE); err != nil {
		return "", fmt.Errorf("promote publication to %s without replacement: %w", destination.Name, err)
	}
	return strings.ReplaceAll(reference, target.Name, destination.Name), nil
}

func program(input api.Program) (coopr.Program, error) {
	output := coopr.Program{Platform: input.Platform, Output: coopr.StateID(input.Output)}
	source := func(s api.Source) coopr.Source {
		return coopr.Source{Image: s.Image, State: coopr.StateID(s.State), Context: s.Context}
	}
	for _, s := range input.States {
		if s.From.Context != "" {
			return coopr.Program{}, errors.New("named contexts cannot supply a state base")
		}
		state := coopr.State{ID: coopr.StateID(s.ID), From: source(s.From)}
		for _, op := range s.Operations {
			allowed := api.Operation{Kind: op.Kind}
			var operation coopr.Operation
			switch op.Kind {
			case "run":
				allowed.Args, allowed.Mounts = op.Args, op.Mounts
				mounts := make([]coopr.Mount, 0, len(op.Mounts))
				for _, m := range op.Mounts {
					if m.From.Context != "" && (m.From.Image != "" || m.From.State != "") {
						return coopr.Program{}, errors.New("RUN mount source selects multiple inputs")
					}
					if m.Relabel != "" && (m.HostPath == "" || (m.Relabel != "shared" && m.Relabel != "private")) {
						return coopr.Program{}, errors.New("relabel requires a live host bind and shared or private mode")
					}
					if m.HostPath != "" && (m.Type != "bind" || m.From != (api.Source{}) || m.Source != "") {
						return coopr.Program{}, errors.New("live host mount requires only a bind hostPath")
					}
					mounts = append(mounts, coopr.Mount{HostPath: m.HostPath, Relabel: m.Relabel, Type: m.Type, Target: m.Target, Source: m.Source, ID: m.ID, Sharing: m.Sharing, From: source(m.From), ReadOnly: m.ReadOnly})
				}
				operation = coopr.Run{Args: op.Args, Mounts: mounts}
			case "copy":
				if op.From.Context != "" && (op.From.Image != "" || op.From.State != "") {
					return coopr.Program{}, errors.New("COPY source selects multiple inputs")
				}
				allowed.Sources, allowed.Destination, allowed.From = op.Sources, op.Destination, op.From
				operation = coopr.Copy{Sources: op.Sources, Destination: op.Destination, From: source(op.From)}
			case "env":
				allowed.Name, allowed.Value = op.Name, op.Value
				operation = coopr.Env{Name: op.Name, Value: op.Value}
			case "workdir":
				allowed.Path = op.Path
				operation = coopr.Workdir{Path: op.Path}
			default:
				return coopr.Program{}, fmt.Errorf("unknown operation kind %q", op.Kind)
			}
			if !reflect.DeepEqual(allowed, op) {
				return coopr.Program{}, fmt.Errorf("operation %q contains incompatible fields", op.Kind)
			}
			state.Operations = append(state.Operations, operation)
		}
		output.States = append(output.States, state)
	}
	return output, nil
}
func within(a, b string) bool {
	rel, err := filepath.Rel(a, b)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
func overlap(a, b string) bool { return within(a, b) || within(b, a) }
func absolute(path string) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("path must be absolute: %q", path)
	}
	return nil
}
func fresh(path string) error {
	if err := absolute(path); err != nil {
		return err
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		if err != nil {
			return err
		}
		return fmt.Errorf("output already exists: %s", path)
	}
	parent, err := os.Stat(filepath.Dir(path))
	if err != nil {
		return err
	}
	if !parent.IsDir() {
		return errors.New("output parent is not a directory")
	}
	return nil
}

// canonical resolves existing parents so a symlink cannot conceal overlap.
func canonical(path string) (string, error) {
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
	resolved, err = canonical(parent)
	if err != nil {
		return "", err
	}
	return filepath.Join(resolved, filepath.Base(path)), nil
}

func validate(r api.Request) error {
	if r.Kind == "component-load" {
		return validateComponentLoad(r)
	}
	if r.ComponentLoad != nil {
		return errors.New("componentLoad requires component-load kind")
	}

	if r.Options.Network != "" && r.Kind == "publish" {
		return errors.New("publish does not accept RUN network options")
	}
	if _, _, err := buildah.NormalizeBuildNetworkOptions(r.Options.Network, nil); err != nil {
		return fmt.Errorf("RUN network: %w", err)
	}

	if _, err := buildah.RegistryCacheSpecs(r.Options.Cache.RegistryFrom); err != nil {
		return fmt.Errorf("registry cache sources: %w", err)
	}
	if _, err := buildah.RegistryCacheSpecs(r.Options.Cache.RegistryTo); err != nil {
		return fmt.Errorf("registry cache destinations: %w", err)
	}
	if r.Options.Jobs < 0 {
		return errors.New("jobs must be nonnegative")
	}
	o := r.Options
	for _, p := range []string{o.Store.RunRoot, o.Store.GraphRoot} {
		if err := absolute(p); err != nil {
			return err
		}
	}
	if overlap(o.Store.RunRoot, o.Store.GraphRoot) {
		return errors.New("native store roots must not overlap")
	}
	for _, p := range []string{o.ContextDir, o.Cache.LocalDir, o.Cache.MountDir, o.Registry.AuthFile, o.Registry.CertDir, o.Registry.SignaturePolicyPath} {
		if p != "" {
			if err := absolute(p); err != nil {
				return err
			}
		}
	}
	if o.ContextDir != "" {
		info, err := os.Stat(o.ContextDir)
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return errors.New("context must be a directory")
		}
	}

	for _, p := range []*string{&o.Store.RunRoot, &o.Store.GraphRoot, &o.ContextDir, &o.Cache.LocalDir, &o.Cache.MountDir, &o.Output.OCI, &o.Output.Filesystem} {
		if *p != "" {
			if err := absolute(*p); err != nil {
				return err
			}
			normalized, err := canonical(*p)
			if err != nil {
				return err
			}
			*p = normalized
		}
	}
	if err := buildah.ValidateFileExports(o.Output.Exports); err != nil {
		return err
	}
	for i := range o.Output.Exports {
		if o.Output.Exports[i].State != "" && r.Kind != "execute" {
			return errors.New("file export state selection requires a typed program")
		}
		normalized, err := canonical(o.Output.Exports[i].Destination)
		if err != nil {
			return err
		}
		o.Output.Exports[i].Destination = normalized
	}
	if overlap(o.Store.RunRoot, o.Store.GraphRoot) {
		return errors.New("native store roots must not overlap")
	}
	if o.ContextDir != "" {
		for _, resource := range []string{o.Store.RunRoot, o.Store.GraphRoot, o.Cache.LocalDir} {
			if resource != "" && overlap(resource, o.ContextDir) {
				return errors.New("context overlaps native store or cache")
			}
		}
	}
	if o.Cache.MountDir != "" {
		for _, resource := range []string{o.ContextDir, o.Store.RunRoot, o.Store.GraphRoot, o.Cache.LocalDir, o.Output.OCI, o.Output.Filesystem} {
			if resource != "" && overlap(o.Cache.MountDir, resource) {
				return errors.New("mount cache directory overlaps context, store, result cache or output")
			}
		}
	}
	selectedComponentStore := ""
	if r.Build != nil || r.Publish != nil {
		selected := ""
		if r.Build != nil {
			selected = r.Build.ComponentStoreDir
		} else {
			selected = r.Publish.ComponentStoreDir
		}
		selected = componentStore(selected, o.Store.GraphRoot)
		if err := absolute(selected); err != nil {
			return err
		}
		selected, err := canonical(selected)
		if err != nil {
			return err
		}
		selectedComponentStore = selected
		for _, resource := range []string{o.ContextDir, o.Store.GraphRoot, o.Store.RunRoot, o.Cache.LocalDir, o.Cache.MountDir} {
			if resource != "" && overlap(selected, resource) {
				return errors.New("component store overlaps context, native storage or cache")
			}
		}
	}

	namedContexts, err := localContexts(o.Contexts)
	if err != nil {
		return err
	}
	inputRoots := []string{o.ContextDir}
	for _, input := range namedContexts {
		for _, resource := range []string{o.Store.RunRoot, o.Store.GraphRoot, o.Cache.LocalDir, o.Cache.MountDir, selectedComponentStore} {
			if resource != "" && overlap(input.Path, resource) {
				return errors.New("named context overlaps native storage, component store or cache")
			}
		}
		inputRoots = append(inputRoots, input.Path)
	}
	resources := append([]string{o.Store.GraphRoot, o.Store.RunRoot, o.Cache.LocalDir, o.Cache.MountDir, selectedComponentStore}, inputRoots...)
	switch r.Kind {
	case "execute":
		if r.Program == nil || r.Build != nil || r.Publish != nil {
			return errors.New("execute requires only program")
		}
		if _, err := program(*r.Program); err != nil {
			return err
		}
		for _, state := range r.Program.States {
			for _, op := range state.Operations {
				for _, mount := range op.Mounts {
					if mount.From.Context != "" {
						if _, known := o.Contexts[mount.From.Context]; !known {
							return fmt.Errorf("unknown RUN context %q", mount.From.Context)
						}
					}
					if mount.HostPath != "" {
						if err := absolute(mount.HostPath); err != nil {
							return fmt.Errorf("live host bind: %w", err)
						}
						host, err := canonical(mount.HostPath)
						if err != nil {
							return fmt.Errorf("live host bind: %w", err)
						}
						info, err := os.Stat(host)
						if err != nil {
							return fmt.Errorf("live host bind %q is unavailable: %w", host, err)
						}
						if !info.IsDir() {
							return errors.New("live host bind must be a directory")
						}
						resources = append(resources, host)
						for _, resource := range []string{o.Store.RunRoot, o.Store.GraphRoot, o.Cache.LocalDir, o.Cache.MountDir, o.Output.OCI, o.Output.Filesystem} {
							if resource != "" && overlap(host, resource) {
								return errors.New("live host bind overlaps store, cache or output")
							}
						}
					}
				}
				if op.Kind == "copy" && op.From.Context != "" {
					if _, known := o.Contexts[op.From.Context]; !known {
						return fmt.Errorf("unknown COPY context %q", op.From.Context)
					}
				}
			}
		}
	case "build", "component":
		if r.Build == nil || r.Program != nil || r.Publish != nil || len(r.Build.Files) == 0 {
			return errors.New("build/component requires only build with definition files")
		}
		if o.ContextDir == "" {
			return errors.New("definition context is required")
		}
		if _, err := normalizeBuildPlatform(r.Build.Platform); err != nil {
			return err
		}
		for _, f := range r.Build.Files {
			if err := absolute(f); err != nil {
				return err
			}
			info, err := os.Stat(f)
			if err != nil {
				return err
			}
			if !info.Mode().IsRegular() {
				return errors.New("definition must be a regular file")
			}
		}
		if r.Build.ComponentStoreDir != "" {
			if err := absolute(r.Build.ComponentStoreDir); err != nil {
				return err
			}
		}
		if r.Kind == "component" && o.Output.Filesystem != "" {
			return errors.New("components export OCI archives, not root filesystems")
		}
	case "publish":
		if r.Publish == nil || r.Program != nil || r.Build != nil {
			return errors.New("publish requires only publish payload")
		}
		if r.Publish.Artifact != "image" && r.Publish.Artifact != "component" {
			return errors.New("publish artifact must be image or component")
		}
		if r.Publish.Source == "" || len(r.Publish.Destinations) == 0 {
			return errors.New("publish source and destinations are required")
		}
		kind := oci.Image
		if r.Publish.Artifact == "component" {
			kind = oci.Component
		}
		if r.Publish.ComponentStoreDir != "" {
			if err := absolute(r.Publish.ComponentStoreDir); err != nil {
				return err
			}
		}
		if strings.HasPrefix(r.Publish.Source, "oci:") {
			source, _, err := publishOCISource(context.Background(), r.Publish)
			if err != nil {
				return err
			}
			for _, resource := range []string{o.Store.GraphRoot, o.Store.RunRoot, o.Cache.LocalDir, o.Cache.MountDir, selectedComponentStore} {
				if resource != "" && overlap(source, resource) {
					return errors.New("OCI publication source overlaps execution storage or cache")
				}
			}
			resources = append(resources, source)
		} else if r.Publish.SourceDigest != "" {
			return errors.New("sourceDigest requires an OCI layout image source")
		}
		for _, d := range r.Publish.Destinations {
			parsed, err := transfer.ParsePushDestination(d, kind)
			if err != nil {
				return err
			}
			if filesystemDestination(parsed) {
				if err := fresh(parsed.Name); err != nil {
					return err
				}
				dest, err := canonical(parsed.Name)
				if err != nil {
					return err
				}
				for _, resource := range resources {
					if resource != "" && overlap(resource, dest) {
						return errors.New("publication destination overlaps execution resources")
					}
				}
			}
		}
		if o.Output.OCI != "" || o.Output.Filesystem != "" || len(o.Output.Exports) != 0 {
			return errors.New("publish uses explicit destinations, not build outputs")
		}
		return nil
	default:
		return fmt.Errorf("unknown request kind %q", r.Kind)
	}
	if o.Output.OCI == "" {
		return errors.New("OCI output is required")
	}
	for _, p := range []string{o.Output.OCI, o.Output.Filesystem} {
		if p != "" {
			if err := fresh(p); err != nil {
				return err
			}
			for _, resource := range resources {
				if resource != "" && overlap(resource, p) {
					return errors.New("output overlaps store, cache or context")
				}
			}
		}
	}
	if o.Output.Filesystem != "" && overlap(o.Output.OCI, o.Output.Filesystem) {
		return errors.New("output paths overlap")
	}
	if len(o.Output.Exports) != 0 && r.Kind != "execute" && r.Kind != "build" {
		return errors.New("selected file exports require an image build or execution")
	}
	for i, export := range o.Output.Exports {
		if err := fresh(export.Destination); err != nil {
			return err
		}
		for _, resource := range append(append([]string(nil), resources...), o.Output.OCI, o.Output.Filesystem) {
			if resource != "" && overlap(resource, export.Destination) {
				return errors.New("selected export overlaps store, cache, context or output")
			}
		}
		for _, previous := range o.Output.Exports[:i] {
			if overlap(previous.Destination, export.Destination) {
				return errors.New("selected export destinations overlap")
			}
		}
	}
	return nil
}

// localContexts admits only explicit local directory inputs, with stable names
// matching the native context namespace. The primary "build" context is reserved.
func localContexts(values map[string]string) ([]buildcontext.Spec, error) {
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	slices.Sort(names)
	result := make([]buildcontext.Spec, 0, len(names))
	for _, name := range names {
		path := values[name]
		if err := absolute(path); err != nil {
			return nil, err
		}
		parsed, err := buildcontext.Parse([]string{name + "=" + path})
		if err != nil {
			return nil, err
		}
		spec := parsed[0]
		if spec.Name != name || name == "build" || spec.Kind != buildcontext.Local {
			return nil, fmt.Errorf("invalid or reserved local context name %q", name)
		}
		spec.Path, err = filepath.EvalSymlinks(path)
		if err != nil {
			return nil, err
		}
		info, err := os.Stat(spec.Path)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("named context %q must be a directory", name)
		}
		result = append(result, spec)
	}
	return result, nil
}

func validateComponentLoad(r api.Request) error {
	if r.ComponentLoad == nil || r.Program != nil || r.Build != nil || r.Publish != nil {
		return errors.New("component-load requires only componentLoad payload")
	}
	if !reflect.DeepEqual(r.Options, api.Options{}) {
		return errors.New("component-load does not accept execution options")
	}
	load := r.ComponentLoad
	if err := componentstore.ValidateTag(load.Name); err != nil {
		return fmt.Errorf("invalid component load name: %w", err)
	}
	for _, p := range []string{load.Archive, load.ComponentStoreDir} {
		if err := absolute(p); err != nil {
			return err
		}
	}
	info, err := os.Stat(load.Archive)
	if err != nil {
		return fmt.Errorf("component archive unavailable: %w", err)
	}
	if !info.Mode().IsRegular() {
		return errors.New("component archive must be a regular file")
	}
	archive, err := canonical(load.Archive)
	if err != nil {
		return err
	}
	store, err := canonical(load.ComponentStoreDir)
	if err != nil {
		return err
	}
	if overlap(archive, store) {
		return errors.New("component archive overlaps component store")
	}
	return nil
}
