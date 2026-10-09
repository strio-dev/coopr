package buildah

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"coopr/internal/imageconfig"
	upstream "go.podman.io/buildah"
	"go.podman.io/buildah/define"
	"go.podman.io/buildah/docker"
)

func TestApplyOperationsPreservesOrderAndOptions(t *testing.T) {
	fake := &recordingBuilder{}
	contextDir := t.TempDir()
	operations := []Operation{
		Env{Name: "mode", Value: "test"},
		Copy{Sources: []string{"one", "two"}, Destination: "/data", Chown: "12:34", Chmod: "0640", Excludes: []string{"*.tmp"}},
		Add{Sources: []string{"archive.tar"}, Destination: "/opt", Checksum: "sha256:abc"},
		Run{Command: []string{"/bin/sh", "-c", "touch /done"}, Env: []string{"LOCAL=yes"}, WorkingDir: "/data", User: "12:34", cacheLockRoot: contextDir, Mounts: []RunMount{
			{Type: "bind", Properties: map[string]string{"source": "input", "target": "/input", "readonly": "true"}},
			{Type: "cache", Properties: map[string]string{"id": "compile", "target": "/cache", "sharing": "locked"}},
			{Type: "tmpfs", Properties: map[string]string{"target": "/tmp", "size": "64m"}},
		}},
		Label{Name: "org.example.ready", Value: "yes"}, WorkDir("/work"), User("1000"),
		Cmd{"serve"}, Entrypoint{"/entry"}, Shell{"/bin/bash", "-c"}, StopSignal("SIGTERM"),
		Healthcheck(imageconfig.Healthcheck{Test: []string{"CMD-SHELL", "true"}, Interval: 30 * time.Second, Retries: 3}),
		OnBuild("RUN make generated"),
	}
	if err := applyOperations(context.Background(), fake, contextDir, operations); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"env:mode=test",
		"copy:false:" + contextDir + ":/data:12:34:0640:*.tmp,one/*.tmp,two/*.tmp:one,two",
		"copy:true:" + contextDir + ":/opt::::archive.tar:sha256:abc",
		"run:/bin/sh,-c,touch /done:LOCAL=yes:/data:12:34",
		"label:org.example.ready=yes", "mkdir:/work:", "workdir:/work", "user:1000", "cmd:serve",
		"entrypoint:/entry", "shell:/bin/bash,-c", "stop:SIGTERM",
		"healthcheck:CMD-SHELL,true:30s:3", "onbuild:RUN make generated",
	}
	if !reflect.DeepEqual(fake.events, want) {
		t.Fatalf("events = %#v, want %#v", fake.events, want)
	}
	wantMounts := []string{
		"type=bind,relabel=private,readonly,source=input,target=/input",
		"type=cache,id=compile,sharing=locked,target=/cache",
		"type=tmpfs,tmpfs-size=64m,target=/tmp",
	}
	if !reflect.DeepEqual(fake.runMounts, wantMounts) || fake.runContext == contextDir || fake.runContext == "" {
		t.Fatalf("RUN mounts = %#v context = %q, want %#v filtered snapshot", fake.runMounts, fake.runContext, wantMounts)
	}
}

func TestApplyOperationsStopsAtCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	fake := &recordingBuilder{}
	err := applyOperations(ctx, fake, "", []Operation{Env{Name: "unreached", Value: "yes"}})
	if err != context.Canceled || len(fake.events) != 0 {
		t.Fatalf("applyOperations() = %v, events %#v", err, fake.events)
	}
}

func TestDockerMetadataOperationsRejectMalformedValues(t *testing.T) {
	tests := []struct {
		operation Operation
		message   string
	}{
		{Healthcheck(imageconfig.Healthcheck{Test: []string{"OTHER", "true"}}), "CMD, CMD-SHELL, or NONE"},
		{Healthcheck(imageconfig.Healthcheck{Test: []string{"CMD", "true"}, Interval: -time.Second}), "interval"},
		{OnBuild(" \t "), "Dockerfile instruction"},
		{OnBuild("RUN one\nRUN two"), "exactly one instruction"},
	}
	for _, test := range tests {
		fake := &recordingBuilder{}
		err := applyOperations(context.Background(), fake, "", []Operation{test.operation})
		if err == nil || !strings.Contains(err.Error(), test.message) || len(fake.events) != 0 {
			t.Errorf("apply(%#v) error = %v, events = %v", test.operation, err, fake.events)
		}
	}
}

func TestOnBuildOperationAcceptsHeredoc(t *testing.T) {
	trigger := "RUN <<EOF\necho inherited > /marker\nEOF"
	fake := &recordingBuilder{}
	if err := applyOperations(context.Background(), fake, "", []Operation{OnBuild(trigger)}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fake.events, []string{"onbuild:" + trigger}) {
		t.Fatalf("events = %#v", fake.events)
	}
}

func TestOnBuildOperationAcceptsSurroundingWhitespace(t *testing.T) {
	fake := &recordingBuilder{}
	if err := applyOperations(context.Background(), fake, "", []Operation{OnBuild("  RUN true  ")}); err != nil {
		t.Fatal(err)
	}
	if len(fake.onBuild) != 1 || fake.onBuild[0] != "  RUN true  " {
		t.Fatalf("stored ONBUILD triggers = %#v", fake.onBuild)
	}
}

func TestCopyRejectsRemoteSourceBeforeCallingBuilder(t *testing.T) {
	fake := &recordingBuilder{}
	err := applyOperations(context.Background(), fake, t.TempDir(), []Operation{
		Copy{Sources: []string{"http://example.invalid/file"}, Destination: "/file"},
	})
	if err == nil || !strings.Contains(err.Error(), "COPY source must be local") || len(fake.events) != 0 {
		t.Fatalf("COPY remote source = %v, builder events = %v", err, fake.events)
	}
}

func TestCopyAndAddPassParentsToBuildah(t *testing.T) {
	fake := &recordingBuilder{}
	err := applyOperations(context.Background(), fake, t.TempDir(), []Operation{
		Copy{Sources: []string{"source/./nested/file"}, Destination: "/copy", Parents: true},
		Add{Sources: []string{"archive/./nested/file"}, Destination: "/add", Parents: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(fake.addOptions) != 2 || !fake.addOptions[0].Parents || !fake.addOptions[1].Parents {
		t.Fatalf("Buildah add/copy options = %#v, want Parents enabled for both operations", fake.addOptions)
	}
}

func TestCopyPassesLinkToBuildah(t *testing.T) {
	for _, linked := range []bool{true, false} {
		t.Run(strconv.FormatBool(linked), func(t *testing.T) {
			fake := &recordingBuilder{}
			err := applyOperations(context.Background(), fake, t.TempDir(), []Operation{
				Copy{Sources: []string{"source"}, Destination: "/copy", Link: linked},
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(fake.addOptions) != 1 || fake.addOptions[0].Link != linked {
				t.Fatalf("Buildah COPY Link = %v, want %v", fake.addOptions[0].Link, linked)
			}
		})
	}
}

func TestAddPassesLinkToBuildah(t *testing.T) {
	for _, linked := range []bool{true, false} {
		t.Run(strconv.FormatBool(linked), func(t *testing.T) {
			fake := &recordingBuilder{}
			err := applyOperations(context.Background(), fake, t.TempDir(), []Operation{
				Add{Sources: []string{"source"}, Destination: "/add", Link: linked},
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(fake.addOptions) != 1 || fake.addOptions[0].Link != linked {
				t.Fatalf("Buildah ADD Link = %v, want %v", fake.addOptions[0].Link, linked)
			}
		})
	}
}

func TestWorkDirResolvesAgainstBaseAndCreatesAsCurrentUser(t *testing.T) {
	fake := &recordingBuilder{workDir: "/base", user: "1000:1000"}
	err := applyOperations(context.Background(), fake, "", []Operation{
		WorkDir("nested"), WorkDir("../next"), WorkDir("/absolute"),
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"mkdir:/base/nested:1000:1000", "workdir:/base/nested",
		"mkdir:/base/next:1000:1000", "workdir:/base/next",
		"mkdir:/absolute:1000:1000", "workdir:/absolute",
	}
	if !reflect.DeepEqual(fake.events, want) {
		t.Fatalf("events = %#v, want %#v", fake.events, want)
	}
}

func TestRunNetworkPolicy(t *testing.T) {
	fake := &recordingBuilder{}
	err := applyOperations(context.Background(), fake, "", []Operation{
		Run{Command: []string{"/bin/true"}},
		Run{Command: []string{"/bin/true"}, Network: "none"},
		Run{Command: []string{"/bin/true"}, Network: "host"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []define.NetworkConfigurationPolicy{define.NetworkEnabled, define.NetworkDisabled, define.NetworkEnabled}
	if !reflect.DeepEqual(fake.runPolicies, want) {
		t.Fatalf("RUN network policies = %#v, want %#v", fake.runPolicies, want)
	}
	host := fake.runOptions[2].NamespaceOptions.Find("network")
	if host == nil || !host.Host || host.Path != "" {
		t.Fatalf("RUN host network namespace = %#v", host)
	}
}

func TestRunForwardsHostFileControls(t *testing.T) {
	fake := &recordingBuilder{noHostname: true, noHosts: true}
	if err := (Run{Command: []string{"/bin/true"}, Network: "none"}).apply(fake, ""); err != nil {
		t.Fatal(err)
	}
	if len(fake.runOptions) != 1 || !fake.runOptions[0].NoHostname || !fake.runOptions[0].NoHosts {
		t.Fatalf("RUN host-file options = %#v", fake.runOptions)
	}
}

func TestRunPassesResolvedCDIDevicesToBuildah(t *testing.T) {
	builder := &recordingBuilder{}
	operation := Run{
		Command: []string{"/bin/true"},
		Devices: []runDeviceRequest{{Name: "accelerator", Required: true}},
	}
	builder.cdiConfigDir = testCDISpecDir(t)
	if err := operation.apply(builder, ""); err != nil {
		t.Fatal(err)
	}
	want := []string{"other.example/device=gamma", "vendor.example/device=beta"}
	if len(builder.runOptions) != 1 || !reflect.DeepEqual(builder.runOptions[0].DeviceSpecs, want) {
		t.Fatalf("Buildah DeviceSpecs = %#v, want %#v", builder.runOptions, want)
	}
}

func TestRunSecurityModes(t *testing.T) {
	fake := &recordingBuilder{}
	if err := applyOperations(context.Background(), fake, "", []Operation{Run{Command: []string{"/bin/true"}, Security: "sandbox"}}); err != nil {
		t.Fatalf("sandbox security mode: %v", err)
	}
	if err := applyOperations(context.Background(), fake, "", []Operation{Run{Command: []string{"/bin/true"}, Security: "insecure"}}); err != nil {
		t.Fatalf("insecure security mode: %v", err)
	}
	if got := fake.runOptions[1].Args; !reflect.DeepEqual(got, []string{insecureRunRequestedMarker}) {
		t.Fatalf("insecure runtime marker = %#v", got)
	}
}

func TestRunMountsAreScopedToOneRun(t *testing.T) {
	fake := &recordingBuilder{}
	err := applyOperations(context.Background(), fake, "/context", []Operation{
		Run{Command: []string{"/bin/true"}, Mounts: []RunMount{{Type: "tmpfs", Properties: map[string]string{"target": "/tmp"}}}},
		Run{Command: []string{"/bin/true"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := [][]string{{"type=tmpfs,target=/tmp"}, {}}
	if !reflect.DeepEqual(fake.runMountHistory, want) {
		t.Fatalf("per-RUN mounts = %#v, want %#v", fake.runMountHistory, want)
	}
}

func TestRunSecretAndSSHInputsReachBuildah(t *testing.T) {
	t.Setenv("COOPR_TEST_SECRET", "ephemeral-value")
	listener, err := net.Listen("unix", filepath.Join(t.TempDir(), "agent.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	fake := &recordingBuilder{}
	err = applyOperations(context.Background(), fake, "", []Operation{
		Run{
			Command: []string{"/bin/true"},
			Mounts: []RunMount{
				{Type: "secret", Properties: map[string]string{"id": "token", "required": "true"}},
				{Type: "ssh", Properties: map[string]string{"id": "default", "required": "true"}},
			},
			SecretSpecs: []string{"id=token,env=COOPR_TEST_SECRET"},
			SSHSpecs:    []string{"default=" + listener.Addr().String()},
		},
		Run{Command: []string{"/bin/true"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(fake.runOptions) != 2 {
		t.Fatalf("RUN options = %d, want 2", len(fake.runOptions))
	}
	first := fake.runOptions[0]
	if secret := first.Secrets["token"]; secret.SourceType != "env" || secret.Source != "COOPR_TEST_SECRET" {
		t.Fatalf("Buildah secret source = %#v", secret)
	}
	if source := first.SSHSources["default"]; source == nil || source.Socket != listener.Addr().String() {
		t.Fatalf("Buildah SSH source = %#v", source)
	}
	if len(fake.runOptions[1].Secrets) != 0 || len(fake.runOptions[1].SSHSources) != 0 {
		t.Fatal("secret or SSH source leaked into the next RUN")
	}
}

func TestRunSSHSourceWithoutPathUsesAgentEnvironment(t *testing.T) {
	first, err := net.Listen("unix", filepath.Join(t.TempDir(), "first.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Close() }()
	second, err := net.Listen("unix", filepath.Join(t.TempDir(), "second.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Close() }()
	t.Setenv("SSH_AUTH_SOCK", second.Addr().String())
	fake := &recordingBuilder{}
	err = applyOperations(context.Background(), fake, "", []Operation{Run{
		Command: []string{"/bin/true"},
		Mounts: []RunMount{
			{Type: "ssh", Properties: map[string]string{"id": "ci"}},
			{Type: "ssh", Properties: map[string]string{"id": "default"}},
		},
		SSHSpecs: []string{"ci=" + first.Addr().String(), "default"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	sources := fake.runOptions[0].SSHSources
	if sources["ci"] == nil || sources["ci"].Socket != first.Addr().String() || sources["default"] == nil || sources["default"].Socket != second.Addr().String() {
		t.Fatalf("RUN SSH source mapping = %#v", sources)
	}
}

func TestRunRepeatedCredentialIDsUseLastSource(t *testing.T) {
	first, err := net.Listen("unix", filepath.Join(t.TempDir(), "first.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Close() }()
	last, err := net.Listen("unix", filepath.Join(t.TempDir(), "last.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = last.Close() }()
	t.Setenv("COOPR_FIRST_SECRET", "first")
	t.Setenv("COOPR_LAST_SECRET", "last")
	fake := &recordingBuilder{}
	err = applyOperations(context.Background(), fake, "", []Operation{Run{
		Command: []string{"/bin/true"},
		Mounts: []RunMount{
			{Type: "secret", Properties: map[string]string{"id": "token"}},
			{Type: "ssh", Properties: map[string]string{"id": "default"}},
		},
		SecretSpecs: []string{"id=token,env=COOPR_FIRST_SECRET", "id=token,env=COOPR_LAST_SECRET"},
		SSHSpecs:    []string{"default=" + first.Addr().String(), "default=" + last.Addr().String()},
	}})
	if err != nil {
		t.Fatal(err)
	}
	options := fake.runOptions[0]
	if options.Secrets["token"].Source != "COOPR_LAST_SECRET" {
		t.Fatalf("secret did not use last source: %+v", options.Secrets)
	}
	if options.SSHSources["default"].Socket != last.Addr().String() {
		t.Fatalf("SSH did not use last source: %+v", options.SSHSources)
	}
}

func TestRunMountValidation(t *testing.T) {
	tests := []struct {
		name, context string
		mount         RunMount
		message       string
	}{
		{"context required", "", RunMount{Type: "bind", Properties: map[string]string{"target": "/src"}}, "requires a build context"},
		{"bind source escape", "/context", RunMount{Type: "bind", Properties: map[string]string{"source": "../host", "target": "/src"}}, "escapes the build context"},
		{"cache id", "/context", RunMount{Type: "cache", Properties: map[string]string{"target": "/cache"}}, "requires a resolved id"},
		{"cache sharing", "/context", RunMount{Type: "cache", Properties: map[string]string{"id": "x", "target": "/cache", "sharing": "global"}}, "sharing"},
		{"readonly", "/context", RunMount{Type: "tmpfs", Properties: map[string]string{"target": "/tmp", "readonly": "sometimes"}}, "true or false"},
		{"secret source", "", RunMount{Type: "secret", Properties: map[string]string{}}, "requires id or target"},
		{"secret required", "", RunMount{Type: "secret", Properties: map[string]string{"id": "token", "required": "sometimes"}}, "required must be true or false"},
		{"ssh env", "", RunMount{Type: "ssh", Properties: map[string]string{"env": "TOKEN"}}, "not supported"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := applyOperations(context.Background(), &recordingBuilder{}, test.context, []Operation{Run{
				Command: []string{"/bin/true"}, Mounts: []RunMount{test.mount},
			}})
			if err == nil || !strings.Contains(err.Error(), test.message) {
				t.Fatalf("want %q, got %v", test.message, err)
			}
		})
	}
}

func TestContextBindAbsoluteSourceIsContextRootRelative(t *testing.T) {
	tests := []struct {
		source, want string
	}{
		{source: "/input", want: "source=input"},
		{source: "/dir/../input", want: "source=input"},
		{source: "/../../input", want: "source=input"},
		{source: "/", want: "source=."},
	}
	for _, test := range tests {
		t.Run(test.source, func(t *testing.T) {
			serialized, err := serializeRunMount(RunMount{Type: "bind", Properties: map[string]string{
				"source": test.source, "target": "/src",
			}}, "/context")
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(serialized, test.want) {
				t.Fatalf("serialized mount = %q, want %q", serialized, test.want)
			}
			if strings.Contains(serialized, "source=/") {
				t.Fatalf("serialized mount exposed an absolute host source: %q", serialized)
			}
		})
	}
}

func TestWritableBindMountSerializesReadwrite(t *testing.T) {
	mount, err := serializeRunMount(RunMount{Type: "bind", Properties: map[string]string{
		"source": "input", "target": "/input", "readonly": "false",
	}}, "/context")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(mount, "readwrite") {
		t.Fatalf("writable bind mount = %q", mount)
	}
}

func TestRunMountOptionAliasesNormalizeToCanonicalBuildahOptions(t *testing.T) {
	tests := []struct {
		name       string
		mount      RunMount
		contextDir string
		contains   []string
	}{
		{"bind", RunMount{Type: "bind", Properties: map[string]string{"src": "input", "dst": "/input", "rw": "true"}}, "/context", []string{"source=input", "target=/input", "readwrite"}},
		{"cache", RunMount{Type: "cache", Properties: map[string]string{"id": "cache", "src": "/seed", "destination": "/cache", "ro": "true"}}, "", []string{"source=/seed", "target=/cache", "readonly"}},
		{"cache writable", RunMount{Type: "cache", Properties: map[string]string{"id": "cache", "target": "/cache", "rw": "true"}}, "", []string{"id=cache", "target=/cache"}},
		{"tmpfs", RunMount{Type: "tmpfs", Properties: map[string]string{"dst": "/tmp"}}, "", []string{"target=/tmp"}},
		{"tmpfs writable", RunMount{Type: "tmpfs", Properties: map[string]string{"target": "/tmp", "readwrite": "true"}}, "", []string{"target=/tmp"}},
		{"secret", RunMount{Type: "secret", Properties: map[string]string{"id": "token", "destination": "/run/token"}}, "", []string{"target=/run/token"}},
		{"secret source", RunMount{Type: "secret", Properties: map[string]string{"source": "token", "target": "/run/token", "readonly": "true"}}, "", []string{"id=token", "target=/run/token"}},
		{"secret src", RunMount{Type: "secret", Properties: map[string]string{"src": "token", "target": "/run/token", "rw": "false"}}, "", []string{"id=token", "target=/run/token"}},
		{"ssh", RunMount{Type: "ssh", Properties: map[string]string{"id": "default", "dst": "/run/ssh.sock"}}, "", []string{"target=/run/ssh.sock"}},
		{"ssh readonly", RunMount{Type: "ssh", Properties: map[string]string{"id": "default", "target": "/run/ssh.sock", "ro": "true"}}, "", []string{"target=/run/ssh.sock"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			serialized, err := serializeRunMount(test.mount, test.contextDir)
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range test.contains {
				if !strings.Contains(serialized, want) {
					t.Fatalf("serialized mount = %q, missing %q", serialized, want)
				}
			}
			for _, alias := range []string{"src=", "dst=", "destination=", "rw=", "ro="} {
				if strings.Contains(serialized, alias) {
					t.Fatalf("serialized mount retained alias %q: %q", alias, serialized)
				}
			}
			if test.name == "cache writable" || test.name == "tmpfs writable" || test.name == "secret source" || test.name == "secret src" || test.name == "ssh readonly" {
				if strings.Contains(serialized, "readonly") || strings.Contains(serialized, "readwrite") {
					t.Fatalf("serialized inherently writable or read-only mount retained mode flag: %q", serialized)
				}
			}
		})
	}
}

func TestRunMountOptionAliasesRejectConflictsAndInvalidReadwrite(t *testing.T) {
	tests := []struct {
		name, mountType string
		properties      map[string]string
		message         string
	}{
		{"source conflict", "bind", map[string]string{"source": "one", "src": "two", "target": "/input"}, `duplicate "source" aliases`},
		{"secret source/id conflict", "secret", map[string]string{"source": "one", "id": "two"}, "both source and id"},
		{"target conflict", "tmpfs", map[string]string{"target": "/one", "dst": "/two"}, `duplicate "target" aliases`},
		{"invalid rw", "bind", map[string]string{"target": "/input", "rw": "sometimes"}, "must be true or false"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := serializeRunMount(RunMount{Type: test.mountType, Properties: test.properties}, "/context")
			if err == nil || !strings.Contains(err.Error(), test.message) {
				t.Fatalf("want %q, got %v", test.message, err)
			}
		})
	}
}

func TestValidateRequestRequiresIsolatedAbsoluteRoots(t *testing.T) {
	request := Request{Store: StoreOptions{RunRoot: "/tmp/run", GraphRoot: "/tmp/graph"}, Output: Output{Path: "/tmp/out"}}
	if err := validateRequest(request); err != nil {
		t.Fatal(err)
	}
	request.Store.GraphRoot = "relative"
	if err := validateRequest(request); err == nil {
		t.Fatal("relative graph root accepted")
	}
	request.Store.GraphRoot = request.Store.RunRoot
	if err := validateRequest(request); err == nil {
		t.Fatal("shared run and graph root accepted")
	}
}

type recordingBuilder struct {
	events          []string
	workDir         string
	user            string
	runPolicies     []define.NetworkConfigurationPolicy
	runMounts       []string
	runMountHistory [][]string
	runContext      string
	runOptions      []upstream.RunOptions
	addOptions      []upstream.AddAndCopyOptions
	healthchecks    []*docker.HealthConfig
	onBuild         []string
	cdiConfigDir    string
	noHostname      bool
	noHosts         bool
}

func (builder *recordingBuilder) cdiConfigDirectory() string { return builder.cdiConfigDir }
func (builder *recordingBuilder) runHostFileControls() (bool, bool) {
	return builder.noHostname, builder.noHosts
}

func (builder *recordingBuilder) run(command []string, options upstream.RunOptions) error {
	builder.runOptions = append(builder.runOptions, options)
	builder.runPolicies = append(builder.runPolicies, options.ConfigureNetwork)
	builder.runMounts = append([]string(nil), options.RunMounts...)
	builder.runMountHistory = append(builder.runMountHistory, append([]string{}, options.RunMounts...))
	builder.runContext = options.ContextDir
	builder.events = append(builder.events, fmt.Sprintf("run:%s:%s:%s:%s", join(command), join(options.Env), options.WorkingDir, options.User))
	return nil
}
func (builder *recordingBuilder) add(destination string, extract bool, options upstream.AddAndCopyOptions, sources ...string) error {
	builder.addOptions = append(builder.addOptions, options)
	event := fmt.Sprintf("copy:%t:%s:%s:%s:%s:%s:%s", extract, options.ContextDir, destination, options.Chown, options.Chmod, join(options.Excludes), join(sources))
	if options.Checksum != "" {
		event += ":" + options.Checksum
	}
	builder.events = append(builder.events, event)
	return nil
}
func (builder *recordingBuilder) SetEnv(name, value string) {
	builder.events = append(builder.events, "env:"+name+"="+value)
}
func (builder *recordingBuilder) SetLabel(name, value string) {
	builder.events = append(builder.events, "label:"+name+"="+value)
}
func (builder *recordingBuilder) SetWorkDir(value string) {
	builder.workDir = value
	builder.events = append(builder.events, "workdir:"+value)
}
func (builder *recordingBuilder) WorkDir() string { return builder.workDir }
func (builder *recordingBuilder) SetUser(value string) {
	builder.user = value
	builder.events = append(builder.events, "user:"+value)
}
func (builder *recordingBuilder) User() string { return builder.user }
func (builder *recordingBuilder) EnsureContainerPathAs(path, user string, _ *os.FileMode) error {
	builder.events = append(builder.events, "mkdir:"+path+":"+user)
	return nil
}
func (builder *recordingBuilder) ensureContainerPathIsDirectory(path, user string) error {
	builder.events = append(builder.events, "mkdir-dir:"+path+":"+user)
	return nil
}
func (builder *recordingBuilder) SetCmd(value []string) {
	builder.events = append(builder.events, "cmd:"+join(value))
}
func (builder *recordingBuilder) SetEntrypoint(value []string) {
	builder.events = append(builder.events, "entrypoint:"+join(value))
}
func (builder *recordingBuilder) SetShell(value []string) {
	builder.events = append(builder.events, "shell:"+join(value))
}
func (builder *recordingBuilder) SetStopSignal(value string) {
	builder.events = append(builder.events, "stop:"+value)
}
func (builder *recordingBuilder) SetPort(value string) {
	builder.events = append(builder.events, "expose:"+value)
}
func (builder *recordingBuilder) AddVolume(value string) {
	builder.events = append(builder.events, "volume:"+value)
}
func (builder *recordingBuilder) SetMaintainer(value string) {
	builder.events = append(builder.events, "maintainer:"+value)
}
func (builder *recordingBuilder) SetHealthcheck(value *docker.HealthConfig) {
	builder.healthchecks = append(builder.healthchecks, value)
	builder.events = append(builder.events, fmt.Sprintf("healthcheck:%s:%s:%d", join(value.Test), value.Interval, value.Retries))
}
func (builder *recordingBuilder) SetOnBuild(value string) {
	builder.onBuild = append(builder.onBuild, value)
	builder.events = append(builder.events, "onbuild:"+value)
}

func join(values []string) string {
	result := ""
	for index, value := range values {
		if index > 0 {
			result += ","
		}
		result += value
	}
	return result
}
