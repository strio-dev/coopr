package buildah

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"coopr/internal/definition"
	"coopr/internal/imageconfig"
	"coopr/internal/onbuildparse"
	"github.com/opencontainers/runtime-spec/specs-go"
	upstream "go.podman.io/buildah"
	"go.podman.io/buildah/copier"
	"go.podman.io/buildah/define"
	"go.podman.io/buildah/docker"
	buildahparse "go.podman.io/buildah/pkg/parse"
	"go.podman.io/buildah/pkg/sshagent"
	"go.podman.io/storage"
)

// Operation is one mutation in a build stage. Only the operation types in this
// package implement it, keeping dispatch exhaustive as the surface grows.
type Operation interface {
	apply(operationBuilder, string) error
}

type operationBuilder interface {
	run([]string, upstream.RunOptions) error
	add(string, bool, upstream.AddAndCopyOptions, ...string) error
	SetEnv(string, string)
	SetLabel(string, string)
	SetWorkDir(string)
	WorkDir() string
	SetUser(string)
	User() string
	EnsureContainerPathAs(string, string, *os.FileMode) error
	ensureContainerPathIsDirectory(string, string) error
	SetCmd([]string)
	SetEntrypoint([]string)
	SetShell([]string)
	SetStopSignal(string)
	SetPort(string)
	AddVolume(string)
	SetMaintainer(string)
	SetHealthcheck(*docker.HealthConfig)
	SetOnBuild(string)
}

// Run executes Command directly. Shell-form callers should explicitly pass
// their desired shell, for example []string{"/bin/sh", "-c", script}.
type Run struct {
	ContextIgnoreFile string
	Command           []string
	InlineFiles       []definition.InlineFile
	Network           string
	Security          string
	Mounts            []RunMount
	Devices           []runDeviceRequest
	SecretSpecs       []string
	SSHSpecs          []string
	Env               []string
	User              string
	WorkingDir        string
	Stdin             io.Reader
	Stdout            io.Writer
	Stderr            io.Writer
	sourceStore       storage.Store
	cacheLockRoot     string
	executionContext  context.Context
}

func (operation Run) apply(builder operationBuilder, contextDir string) (retErr error) {
	if len(operation.Command) == 0 {
		return errors.New("RUN command is empty")
	}
	network, namespaces, err := runNetworkOptions(operation.Network)
	if err != nil {
		return fmt.Errorf("RUN network: %w", err)
	}
	switch operation.Security {
	case "", "sandbox":
	case "insecure":
	default:
		return fmt.Errorf("unsupported RUN security mode %q", operation.Security)
	}
	var deviceSpecs []string
	cdiConfigDir := ""
	var cdiSpecDirs []string
	if len(operation.Devices) != 0 {
		if provider, ok := builder.(interface{ cdiConfigDirectory() string }); ok {
			cdiConfigDir = provider.cdiConfigDirectory()
		}
		if provider, ok := builder.(interface{ cdiSpecDirectories() []string }); ok {
			cdiSpecDirs = provider.cdiSpecDirectories()
		}
		deviceCache, err := newRunDeviceCacheWithDirs(cdiConfigDir, cdiSpecDirs)
		if err != nil {
			return fmt.Errorf("load CDI devices: %w", err)
		}
		deviceSpecs, err = resolveRunDeviceSpecs(deviceCache, operation.Devices)
		if err != nil {
			return fmt.Errorf("resolve RUN devices: %w", err)
		}
	}
	runMounts, err := serializeRunMounts(operation.Mounts, contextDir)
	if err != nil {
		return err
	}
	var secretSpecs, sshSpecs []string
	for _, mount := range operation.Mounts {
		switch mount.Type {
		case "secret":
			secretSpecs = operation.SecretSpecs
		case "ssh":
			sshSpecs = operation.SSHSpecs
		}
	}
	secrets, err := parseOperationSecrets(secretSpecs)
	if err != nil {
		return fmt.Errorf("parse RUN secrets: %w", err)
	}
	sshSources, err := parseOperationSSH(sshSpecs)
	if err != nil {
		return fmt.Errorf("parse RUN SSH sources: %w", err)
	}
	var directMounts []specs.Mount
	if len(operation.InlineFiles) != 0 {
		directory, cleanup, err := materializeRunInlineFiles(operation.InlineFiles)
		if err != nil {
			return err
		}
		defer func() { retErr = errors.Join(retErr, cleanup()) }()
		directMounts = append(directMounts, specs.Mount{Source: directory, Destination: "/run/coopr-heredoc", Type: "bind", Options: []string{"ro", "bind", "exec", "Z"}})
	}
	if operation.sourceStore != nil {
		native, ok := builder.(nativeBuilder)
		if !ok {
			return errors.New("stage-backed RUN mounts require a native Buildah builder")
		}
		mountedImages := []string{}
		defer func() {
			for index := len(mountedImages) - 1; index >= 0; index-- {
				if _, err := operation.sourceStore.UnmountImage(mountedImages[index], false); err != nil {
					retErr = errors.Join(retErr, fmt.Errorf("unmount RUN source image %s: %w", mountedImages[index], err))
				}
			}
		}()
		for index, mount := range operation.Mounts {
			if mount.Type != "bind" || !mount.BoundFrom {
				continue
			}
			imageID := mount.Properties["from"]
			root, err := operation.sourceStore.MountImage(imageID, nil, native.MountLabel)
			if err != nil {
				return fmt.Errorf("mount RUN source image %s: %w", imageID, err)
			}
			mountedImages = append(mountedImages, imageID)
			source, err := copier.Eval(root, root+string(filepath.Separator)+mount.Properties["source"], copier.EvalOptions{})
			if err != nil {
				return fmt.Errorf("resolve RUN source %q in stage image: %w", mount.Properties["source"], err)
			}
			info, err := os.Stat(source)
			if err != nil {
				return fmt.Errorf("stat RUN source %q in stage image: %w", mount.Properties["source"], err)
			}
			if info.IsDir() {
				continue
			}
			if !info.Mode().IsRegular() {
				return fmt.Errorf("RUN stage bind source %q is neither a directory nor a regular file", mount.Properties["source"])
			}
			mountSource := source
			mountOptions := []string{"ro"}
			if value := mount.Properties["readonly"]; value != "" {
				readonly, _ := strconv.ParseBool(value) // serializeRunMounts validated the value.
				if !readonly {
					snapshot, cleanup, err := snapshotWritableBindFile(source)
					if err != nil {
						return fmt.Errorf("snapshot writable RUN stage bind file %q: %w", mount.Properties["source"], err)
					}
					defer func() { retErr = errors.Join(retErr, cleanup()) }()
					mountSource = snapshot
					mountOptions = []string{"rw", "Z"}
				}
			}
			target := mount.Properties["target"]
			if !path.IsAbs(target) {
				workDir := builder.WorkDir()
				if workDir == "" {
					workDir = "/"
				}
				target = path.Join(workDir, target)
			}
			directMounts = append(directMounts, specs.Mount{Source: mountSource, Destination: target, Type: "bind", Options: mountOptions})
			runMounts[index] = ""
		}
		runMounts = slices.DeleteFunc(runMounts, func(mount string) bool { return mount == "" })
	}
	runOptions := upstream.RunOptions{
		Env: append([]string(nil), operation.Env...), User: operation.User, WorkingDir: operation.WorkingDir,
		Stdin: operation.Stdin, Stdout: operation.Stdout, Stderr: operation.Stderr, ConfigureNetwork: network, NamespaceOptions: namespaces,
		ContextDir: contextDir, RunMounts: runMounts, Mounts: directMounts,
		Secrets: secrets, SSHSources: sshSources, DeviceSpecs: deviceSpecs, CDIConfigDir: cdiConfigDir,
	}
	if provider, ok := builder.(interface{ runHostFileControls() (bool, bool) }); ok {
		runOptions.NoHostname, runOptions.NoHosts = provider.runHostFileControls()
	}
	if operation.Security == "insecure" {
		runOptions.Args = []string{insecureRunRequestedMarker}
	}
	lockContext := operation.executionContext
	if lockContext == nil {
		lockContext = context.Background()
	}
	cacheLocks, err := lockRunCacheMounts(lockContext, operation.cacheLockRoot, operation.Mounts)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, cacheLocks.close()) }()
	return builder.run(append([]string(nil), operation.Command...), runOptions)
}

func parseOperationSecrets(specs []string) (map[string]define.Secret, error) {
	return buildahparse.Secrets(specs)
}

func parseOperationSSH(specs []string) (map[string]*sshagent.Source, error) {
	sources := make(map[string]*sshagent.Source, len(specs))
	for _, spec := range specs {
		// Parse each source independently: the upstream batch parser retains
		// earlier paths when a later source should use SSH_AUTH_SOCK.
		parsed, err := buildahparse.SSH([]string{spec})
		if err != nil {
			return nil, err
		}
		for id, source := range parsed {
			sources[id] = source
		}
	}
	return sources, nil
}

func (builder nativeBuilder) cdiConfigDirectory() string {
	return builder.CDIConfigDir
}

// Copy copies local context content without archive extraction.
type Copy struct {
	IgnoreFile  string
	Sources     []string
	InlineFiles []definition.InlineFile
	Destination string
	Chown       string
	Chmod       string
	Link        bool
	Parents     bool
	Excludes    []string
}

func (operation Copy) apply(builder operationBuilder, contextDir string) error {
	return applyOperationWithContext(builder, contextDir, nil, operation)
}

// Add copies local or remote content and enables Buildah's archive extraction.
type Add struct {
	IgnoreFile  string
	Sources     []string
	InlineFiles []definition.InlineFile
	Destination string
	Chown       string
	Chmod       string
	Checksum    string
	KeepGitDir  *bool
	Unpack      *bool
	Link        bool
	Parents     bool
	Excludes    []string
}

func (operation Add) apply(builder operationBuilder, contextDir string) error {
	return applyOperationWithContext(builder, contextDir, nil, operation)
}

func (operation Add) applyWithPolicy(ctx context.Context, builder operationBuilder, policy contextPolicy) error {
	if len(operation.Sources) == 0 && len(operation.InlineFiles) == 0 || operation.Destination == "" {
		return errors.New("ADD requires at least one source and a destination")
	}
	if len(operation.Sources)+len(operation.InlineFiles) > 1 && (len(operation.InlineFiles) != 0 || hasSpecialAddSource(operation.Sources)) {
		if err := ensureSplitMultipleSourceDestination(builder, operation.Destination, operation.Chown); err != nil {
			return err
		}
	}
	if len(operation.Sources) != 0 {
		if err := applyAddSources(ctx, builder, policy, operation, upstream.AddAndCopyOptions{
			Chown: operation.Chown, Chmod: operation.Chmod,
			Checksum: operation.Checksum, Link: operation.Link, Parents: operation.Parents,
			Excludes: append([]string(nil), operation.Excludes...),
		}); err != nil {
			return err
		}
	}
	for _, source := range operation.InlineFiles {
		if err := addInlineData(builder, operation.Destination, source, upstream.AddAndCopyOptions{
			Chown: operation.Chown, Chmod: operation.Chmod, Link: operation.Link,
			Excludes: append([]string(nil), operation.Excludes...),
		}); err != nil {
			return err
		}
	}
	return nil
}

func (operation Copy) applyWithPolicy(builder operationBuilder, policy contextPolicy) error {
	if len(operation.Sources) == 0 && len(operation.InlineFiles) == 0 || operation.Destination == "" {
		return errors.New("COPY requires at least one source and a destination")
	}
	if len(operation.InlineFiles) != 0 && len(operation.Sources)+len(operation.InlineFiles) > 1 {
		if err := ensureSplitMultipleSourceDestination(builder, operation.Destination, operation.Chown); err != nil {
			return err
		}
	}
	if len(operation.Sources) != 0 {
		if err := requireLocalCopySources(operation.Sources); err != nil {
			return err
		}
		options, sources, err := policy.applyLocalCopy(upstream.AddAndCopyOptions{
			Chown: operation.Chown, Chmod: operation.Chmod,
			Link: operation.Link, Parents: operation.Parents, Excludes: append([]string(nil), operation.Excludes...),
		}, operation.Sources)
		if err != nil {
			return err
		}
		if err := builder.add(operation.Destination, false, options, sources...); err != nil {
			return err
		}
	}
	for _, source := range operation.InlineFiles {
		if err := addInlineData(builder, operation.Destination, source, upstream.AddAndCopyOptions{
			Chown: operation.Chown, Chmod: operation.Chmod, Link: operation.Link,
			Excludes: append([]string(nil), operation.Excludes...),
		}); err != nil {
			return err
		}
	}
	return nil
}

func ensureSplitMultipleSourceDestination(builder operationBuilder, destination, chown string) error {
	resolved := destination
	if !path.IsAbs(resolved) {
		workDir := builder.WorkDir()
		if workDir == "" {
			workDir = "/"
		}
		resolved = path.Join(workDir, resolved)
	}
	resolved = path.Clean(resolved)
	if err := builder.ensureContainerPathIsDirectory(resolved, chown); err != nil {
		return fmt.Errorf("prepare multiple-source destination %q as a directory: %w", destination, err)
	}
	return nil
}

func requireLocalCopySources(sources []string) error {
	for _, source := range sources {
		if strings.HasPrefix(source, "http://") || strings.HasPrefix(source, "https://") {
			return fmt.Errorf("COPY source must be local: %q", source)
		}
	}
	return nil
}

// applyOperationWithContext applies executor-owned context exclusions only to
// local COPY/ADD. COPY/ADD --from operations have their own mounted source and
// deliberately bypass this policy.
func applyOperationWithContext(builder operationBuilder, contextDir string, artifacts []string, operation Operation) error {
	return applyOperationWithContextContext(context.Background(), builder, contextDir, artifacts, operation)
}

func applyOperationWithContextContext(ctx context.Context, builder operationBuilder, contextDir string, artifacts []string, operation Operation) error {
	switch local := operation.(type) {
	case Copy:
		if len(local.InlineFiles) != 0 && len(local.Sources) == 0 {
			return local.applyWithPolicy(builder, contextPolicy{})
		}
		policy, err := prepareContextPolicyWithIgnore(contextDir, artifacts, local.IgnoreFile)
		if err != nil {
			return err
		}
		return local.applyWithPolicy(builder, policy)
	case Add:
		if len(local.InlineFiles) != 0 && len(local.Sources) == 0 {
			return local.applyWithPolicy(ctx, builder, contextPolicy{})
		}
		policy, err := prepareContextPolicyWithIgnore(contextDir, artifacts, local.IgnoreFile)
		if err != nil {
			return err
		}
		return local.applyWithPolicy(ctx, builder, policy)
	case Run:
		local.executionContext = ctx
		return applyRunWithFilteredContext(builder, contextDir, artifacts, local)
	default:
		return operation.apply(builder, contextDir)
	}
}

type Env struct{ Name, Value string }

func (operation Env) apply(builder operationBuilder, _ string) error {
	if operation.Name == "" || strings.Contains(operation.Name, "=") {
		return fmt.Errorf("invalid ENV name %q", operation.Name)
	}
	builder.SetEnv(operation.Name, operation.Value)
	return nil
}

type Label struct{ Name, Value string }

func (operation Label) apply(builder operationBuilder, _ string) error {
	if operation.Name == "" {
		return errors.New("LABEL name is empty")
	}
	builder.SetLabel(operation.Name, operation.Value)
	return nil
}

type WorkDir string

func (operation WorkDir) apply(builder operationBuilder, _ string) error {
	requested := string(operation)
	if requested == "" {
		return errors.New("WORKDIR path is empty")
	}
	resolved := requested
	if !path.IsAbs(resolved) {
		current := builder.WorkDir()
		if current == "" {
			current = "/"
		}
		resolved = path.Join(current, resolved)
	}
	resolved = path.Clean(resolved)
	if err := builder.EnsureContainerPathAs(resolved, builder.User(), nil); err != nil {
		return fmt.Errorf("create WORKDIR %q: %w", resolved, err)
	}
	builder.SetWorkDir(resolved)
	return nil
}

type User string

func (operation User) apply(builder operationBuilder, _ string) error {
	builder.SetUser(string(operation))
	return nil
}

type Cmd []string

func (operation Cmd) apply(builder operationBuilder, _ string) error {
	builder.SetCmd(append([]string(nil), operation...))
	return nil
}

type Entrypoint []string

func (operation Entrypoint) apply(builder operationBuilder, _ string) error {
	builder.SetEntrypoint(append([]string(nil), operation...))
	return nil
}

type Shell []string

func (operation Shell) apply(builder operationBuilder, _ string) error {
	builder.SetShell(append([]string(nil), operation...))
	return nil
}

type StopSignal string

func (operation StopSignal) apply(builder operationBuilder, _ string) error {
	builder.SetStopSignal(string(operation))
	return nil
}

type Expose string

func (operation Expose) apply(builder operationBuilder, _ string) error {
	port := string(operation)
	if port == "" {
		return errors.New("EXPOSE port is empty")
	}
	builder.SetPort(port)
	return nil
}

type Volume string

func (operation Volume) apply(builder operationBuilder, _ string) error {
	volume := string(operation)
	if volume == "" {
		return errors.New("VOLUME path is empty")
	}
	builder.AddVolume(volume)
	return nil
}

type Maintainer string

func (operation Maintainer) apply(builder operationBuilder, _ string) error {
	maintainer := string(operation)
	if maintainer == "" {
		return errors.New("MAINTAINER value is empty")
	}
	builder.SetMaintainer(maintainer)
	return nil
}

type Healthcheck imageconfig.Healthcheck

func (operation Healthcheck) apply(builder operationBuilder, _ string) error {
	logical := imageconfig.New()
	if err := logical.Apply(healthcheckInstruction(operation)); err != nil {
		return err
	}
	healthcheck, err := logical.Healthcheck()
	if err != nil {
		return err
	}
	builder.SetHealthcheck(dockerHealthcheck(healthcheck))
	return nil
}

type OnBuild string

func (operation OnBuild) apply(builder operationBuilder, _ string) error {
	trigger := string(operation)
	if strings.TrimSpace(trigger) == "" || strings.ContainsRune(trigger, '\x00') {
		return errors.New("ONBUILD trigger must be one nonempty Dockerfile instruction without NUL")
	}
	if _, err := onbuildparse.Parse(trigger); err != nil {
		return fmt.Errorf("validate ONBUILD trigger: %w", err)
	}
	builder.SetOnBuild(trigger)
	return nil
}

func healthcheckInstruction(operation Healthcheck) definition.Instruction {
	healthcheck := imageconfig.Healthcheck(operation)
	instruction := definition.Instruction{Name: "healthcheck", Arguments: slices.Clone(healthcheck.Test)}
	for _, property := range []struct {
		name  string
		value fmt.Stringer
	}{
		{"interval", healthcheck.Interval}, {"timeout", healthcheck.Timeout},
		{"start-period", healthcheck.StartPeriod}, {"start-interval", healthcheck.StartInterval},
	} {
		if property.value.String() != "0s" {
			if instruction.Properties == nil {
				instruction.Properties = make(map[string]string)
			}
			instruction.Properties[property.name] = property.value.String()
		}
	}
	if healthcheck.Retries != 0 {
		if instruction.Properties == nil {
			instruction.Properties = make(map[string]string)
		}
		instruction.Properties["retries"] = strconv.Itoa(healthcheck.Retries)
	}
	return instruction
}

func dockerHealthcheck(config *imageconfig.Healthcheck) *docker.HealthConfig {
	if config == nil {
		return nil
	}
	return &docker.HealthConfig{
		Test: slices.Clone(config.Test), Interval: config.Interval, Timeout: config.Timeout,
		StartPeriod: config.StartPeriod, StartInterval: config.StartInterval, Retries: config.Retries,
	}
}

func applyOperations(ctx context.Context, builder operationBuilder, contextDir string, operations []Operation) error {
	return applyOperationsWithArtifacts(ctx, builder, contextDir, nil, operations)
}

func applyOperationsWithArtifacts(ctx context.Context, builder operationBuilder, contextDir string, artifacts []string, operations []Operation) error {
	for index, operation := range operations {
		if err := ctx.Err(); err != nil {
			return err
		}
		if operation == nil {
			return fmt.Errorf("operation %d is nil", index+1)
		}
		if err := applyOperationWithContextContext(ctx, builder, contextDir, artifacts, operation); err != nil {
			return fmt.Errorf("operation %d: %w", index+1, err)
		}
	}
	return nil
}
