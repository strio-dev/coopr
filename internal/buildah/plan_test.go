package buildah

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"coopr/internal/definition"
	"coopr/internal/imageconfig"
	"coopr/internal/planner"
)

func TestRequestFromPlanPreservesInstructionAndCommandForms(t *testing.T) {
	plan := testPlan(t, `
arg "mode" "test"
from "scratch"
arg "mode"
env A="one" B="two"
copy "one/./nested" "two/./nested" "/data/" chown="12:34" chmod="0640" parents="true" { exclude "*.tmp" }
shell "/bin/bash" "-ceu"
run "printf '%s' \"$mode\""
run { exec "/bin/echo" "ok" }
label "org.example.ready" "yes"
workdir "/data"
user "12:34"
cmd "serve"
entrypoint { exec "/entry" "--flag" }
stopsignal "SIGTERM"
`)
	request, err := RequestFromPlan(plan, PlanOptions{ContextDir: "/context"})
	if err != nil {
		t.Fatal(err)
	}
	if request.Base != "scratch" || len(request.Operations) != 12 {
		t.Fatalf("request = %+v", request)
	}
	wantTypes := []any{
		Env{}, Env{}, Copy{}, Shell{}, Run{}, Run{}, Label{}, WorkDir(""), User(""), Cmd{}, Entrypoint{}, StopSignal(""),
	}
	for index, want := range wantTypes {
		if reflect.TypeOf(request.Operations[index]) != reflect.TypeOf(want) {
			t.Fatalf("operation %d type = %T, want %T", index+1, request.Operations[index], want)
		}
	}
	copyOperation := request.Operations[2].(Copy)
	if !reflect.DeepEqual(copyOperation.Sources, []string{"one/./nested", "two/./nested"}) || !reflect.DeepEqual(copyOperation.Excludes, []string{"*.tmp"}) || !copyOperation.Parents {
		t.Fatalf("COPY = %+v", copyOperation)
	}
	shellRun := request.Operations[4].(Run)
	if !reflect.DeepEqual(shellRun.Command, []string{"/bin/bash", "-ceu", `printf '%s' "$mode"`}) || !reflect.DeepEqual(shellRun.Env, []string{"mode=test"}) {
		t.Fatalf("shell RUN = %+v", shellRun)
	}
	execRun := request.Operations[5].(Run)
	if !reflect.DeepEqual(execRun.Command, []string{"/bin/echo", "ok"}) {
		t.Fatalf("exec RUN = %+v", execRun)
	}
	if got := request.Operations[9].(Cmd); !reflect.DeepEqual(got, Cmd{"/bin/bash", "-ceu", "serve"}) {
		t.Fatalf("shell CMD = %#v", got)
	}
}

func TestRequestFromPlanDoesNotDuplicateGlobalMounts(t *testing.T) {
	mounts, err := ParseTransientRunMounts([]string{"type=bind,target=/global"})
	if err != nil {
		t.Fatal(err)
	}
	def, err := definition.Parse(strings.NewReader("from \"scratch\"\nrun \"true\" { mount \"bind\" target=\"/authored\" }\n"))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := planner.Create(def, planner.Options{Mode: planner.Build, Platform: runtime.GOOS + "/" + runtime.GOARCH, TransientRunMounts: TransientMountInstructions(mounts)})
	if err != nil {
		t.Fatal(err)
	}
	request, err := RequestFromPlan(plan, PlanOptions{ContextDir: "/context", IgnoreFile: "/custom.ignore", TransientRunMounts: mounts})
	if err != nil {
		t.Fatal(err)
	}
	run := request.Operations[0].(Run)
	counts := map[string]int{}
	for _, mount := range append(run.Mounts, request.TransientRunMounts...) {
		counts[mount.Properties["target"]]++
	}
	if counts["/global"] != 1 || counts["/authored"] != 1 || len(counts) != 2 {
		t.Fatalf("effective mount counts=%v", counts)
	}
	if request.IgnoreFile != "/custom.ignore" || run.ContextIgnoreFile != request.IgnoreFile {
		t.Fatalf("adapter lost explicit context policy: %+v", request)
	}
}

func TestRequestFromPlanPreservesAddGitAndUnpackOptions(t *testing.T) {
	plan := testPlan(t, `
from "scratch"
add "https://example.invalid/repository.git#main" "/source/" keep-git-dir="true" checksum="abcdef"
add "archive.tar" "/archive/" unpack="false"
`)
	request, err := RequestFromPlan(plan, PlanOptions{ContextDir: "/context"})
	if err != nil {
		t.Fatal(err)
	}
	gitAdd := request.Operations[0].(Add)
	if gitAdd.KeepGitDir == nil || !*gitAdd.KeepGitDir || gitAdd.Checksum != "abcdef" || gitAdd.Unpack != nil {
		t.Fatalf("Git ADD = %+v", gitAdd)
	}
	archiveAdd := request.Operations[1].(Add)
	if archiveAdd.Unpack == nil || *archiveAdd.Unpack || archiveAdd.KeepGitDir != nil {
		t.Fatalf("archive ADD = %+v", archiveAdd)
	}
}

func TestRequestFromPlanRejectsInvalidAddBooleanOptions(t *testing.T) {
	for _, property := range []string{"keep-git-dir", "unpack"} {
		t.Run(property, func(t *testing.T) {
			_, err := RequestFromPlan(testPlan(t, "from \"scratch\"\nadd \"source\" \"/source\" "+property+"=\"sometimes\"\n"), PlanOptions{})
			if err == nil || !strings.Contains(err.Error(), property+" must be true or false") {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestRequestFromPlanRejectsUnsupportedSemantics(t *testing.T) {
	tests := []struct {
		name, source, message string
	}{
		{"non-scratch base", `from "alpine:3.20"`, "must be from scratch"},
		{"multiple stages", "from \"scratch\" as=\"base\"\nfrom \"base\"\n", "exactly one stage"},
		{"run mount without cache scope", "from \"scratch\"\nrun \"true\" { mount \"cache\" target=\"/cache\" }\n", "requires executor scope"},
		{"copy invalid parents", "from \"scratch\"\ncopy \"a\" \"/a\" parents=\"sometimes\"\n", "parents must be true or false"},
		{"copy remote URL", "from \"scratch\"\ncopy \"https://example.invalid/file\" \"/file\"\n", "COPY source must be local"},
		{"unsupported instruction", "from \"scratch\"\nfrobnicate \"value\"\n", "instruction is not supported"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := RequestFromPlan(testPlan(t, test.source), PlanOptions{})
			if err == nil || !strings.Contains(err.Error(), test.message) {
				t.Fatalf("want %q, got %v", test.message, err)
			}
		})
	}
}

func TestRequestFromPlanLowersRunHostNetworkAndSandboxSecurity(t *testing.T) {
	request, err := RequestFromPlan(testPlan(t, "from \"scratch\"\nrun \"true\" network=\"host\" security=\"sandbox\"\n"), PlanOptions{Allow: []string{"network.host"}})
	if err != nil {
		t.Fatal(err)
	}
	run := request.Operations[0].(Run)
	if run.Network != "host" || run.Security != "sandbox" || !reflect.DeepEqual(request.Allow, []string{"network.host"}) {
		t.Fatalf("lowered RUN = %#v; allow = %#v", run, request.Allow)
	}
}

func TestRequestFromPlanLowersNativeRunNetworkModes(t *testing.T) {
	for _, network := range []string{"private", "slirp4netns:mtu=1400", "ns:/run/netns/example", "buildnet"} {
		request, err := RequestFromPlan(testPlan(t, "from \"scratch\"\nrun \"true\" network=\""+network+"\"\n"), PlanOptions{})
		if err != nil {
			t.Fatalf("lower %s: %v", network, err)
		}
		if got := request.Operations[0].(Run).Network; got != network {
			t.Fatalf("lowered network = %q, want %q", got, network)
		}
	}
}

func TestRequestFromPlanValidatesDNSAgainstEffectiveRunNetwork(t *testing.T) {
	controls := RunControls{DNSServers: []string{"1.1.1.1"}}
	if _, err := RequestFromPlan(testPlan(t, "from \"scratch\"\nrun \"true\" network=\"none\"\n"), PlanOptions{Network: "default", RunControls: controls}); err == nil || !strings.Contains(err.Error(), "DNS controls") {
		t.Fatalf("authored network none with DNS = %v", err)
	}
	request, err := RequestFromPlan(testPlan(t, "from \"scratch\"\nrun \"true\" network=\"default\"\n"), PlanOptions{Network: "none", RunControls: controls})
	if err != nil {
		t.Fatalf("explicit network override rejected: %v", err)
	}
	if got := request.Operations[0].(Run).Network; got != "default" {
		t.Fatalf("effective network = %q", got)
	}
}

func TestRequestFromPlanLowersRunInsecureSecurity(t *testing.T) {
	request, err := RequestFromPlan(testPlan(t, "from \"scratch\"\nrun \"true\" security=\"insecure\"\n"), PlanOptions{Allow: []string{"security.insecure"}})
	if err != nil {
		t.Fatal(err)
	}
	run := request.Operations[0].(Run)
	if run.Security != "insecure" || !reflect.DeepEqual(request.Allow, []string{"security.insecure"}) {
		t.Fatalf("lowered RUN = %#v; allow = %#v", run, request.Allow)
	}
}

func TestLowerMultipleCopySourcesDefersDestinationTypeToBuilder(t *testing.T) {
	instruction := definition.Instruction{
		Name: "copy", Arguments: []string{"one", "two", "/target"},
	}
	lowered, _, err := lowerCopyOrAdd(instruction, "build")
	if err != nil {
		t.Fatal(err)
	}
	copyOperation := lowered[0].(Copy)
	if !reflect.DeepEqual(copyOperation.Sources, []string{"one", "two"}) {
		t.Fatalf("multiple-source lowering = %#v", copyOperation)
	}
	if copyOperation.Destination != "/target" {
		t.Fatalf("multiple-source destination = %q", copyOperation.Destination)
	}
}

func TestLowerImportedInlineSourcesAndMultilineShebang(t *testing.T) {
	copyOperations, _, err := lowerCopyOrAdd(definition.Instruction{
		Name: "copy", Arguments: []string{"/messages/"},
		InlineFiles: []definition.InlineFile{{Path: "one", Data: "first"}, {Path: "two", Data: "second"}},
	}, "build")
	if err != nil {
		t.Fatal(err)
	}
	request, err := RequestFromPlan(testPlan(t, `
from "scratch"
run """
    #!/bin/sh
    true
    """
`), PlanOptions{ContextDir: "/context"})
	if err != nil {
		t.Fatal(err)
	}
	copyOperation, copyOK := copyOperations[0].(Copy)
	runOperation, runOK := request.Operations[0].(Run)
	if !copyOK || !reflect.DeepEqual(copyOperation.InlineFiles, []definition.InlineFile{{Path: "one", Data: "first"}, {Path: "two", Data: "second"}}) {
		t.Fatalf("lowered imported COPY = %#v", copyOperations[0])
	}
	if !runOK || !reflect.DeepEqual(runOperation.Command, []string{"/run/coopr-heredoc/script"}) || len(runOperation.InlineFiles) != 1 {
		t.Fatalf("lowered shebang RUN = %#v", request.Operations[0])
	}
}

func TestLowerHeredocChecksumRequiresPathSource(t *testing.T) {
	instruction := definition.Instruction{
		Name: "add", Arguments: []string{"/target"}, Properties: map[string]string{"checksum": "sha256:abc"},
		InlineFiles: []definition.InlineFile{{Path: "PAYLOAD", Data: "inline\n"}},
	}
	if _, _, err := lowerCopyOrAdd(instruction, "build"); err == nil || !strings.Contains(err.Error(), "exactly one path source") {
		t.Fatalf("lower checksum heredoc error = %v", err)
	}
}

func TestRequestFromPlanLowersRepeatedRunDevicesAlongsideMounts(t *testing.T) {
	request, err := RequestFromPlan(testPlan(t, `
from "scratch"
run "true" {
  device "vendor.example/device=one,required"
  mount "tmpfs" target="/tmp"
  device "accelerator,required=false"
}
`), PlanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	run := request.Operations[0].(Run)
	wantDevices := []runDeviceRequest{
		{Name: "vendor.example/device=one", Required: true},
		{Name: "accelerator", Required: false},
	}
	if !reflect.DeepEqual(run.Devices, wantDevices) {
		t.Fatalf("RUN devices = %#v, want %#v", run.Devices, wantDevices)
	}
	if len(run.Mounts) != 1 || run.Mounts[0].Type != "tmpfs" {
		t.Fatalf("RUN mounts = %#v", run.Mounts)
	}
}

func TestRequestFromPlanRejectsInvalidRunDeviceOption(t *testing.T) {
	_, err := RequestFromPlan(testPlan(t, `from "scratch"
run "true" { device "name=vendor.example/device=one,required=sometimes" }
`), PlanOptions{})
	if err == nil || !strings.Contains(err.Error(), "invalid value for required") {
		t.Fatalf("invalid RUN device error = %v", err)
	}
}

func TestRequestFromPlanBindsCredentialSourceSpecs(t *testing.T) {
	plan := testPlan(t, `
from "scratch"
run "true" { mount "secret" id="token" required="true"; mount "ssh" id="default" required="true" }
`)
	request, err := RequestFromPlan(plan, PlanOptions{
		Secrets: []string{"id=token,env=TOKEN"}, SSH: []string{"default=/tmp/agent.sock"},
	})
	if err != nil {
		t.Fatal(err)
	}
	run, ok := request.Operations[0].(Run)
	if !ok || !reflect.DeepEqual(run.SecretSpecs, []string{"id=token,env=TOKEN"}) || !reflect.DeepEqual(run.SSHSpecs, []string{"default=/tmp/agent.sock"}) {
		t.Fatalf("lowered credential RUN = %#v", request.Operations[0])
	}
	if !reflect.DeepEqual(request.Secrets, []string{"id=token,env=TOKEN"}) || !reflect.DeepEqual(request.SSH, []string{"default=/tmp/agent.sock"}) {
		t.Fatalf("request credential sources = secrets %v, SSH %v", request.Secrets, request.SSH)
	}
}

func TestRequestFromPlanLowersHealthcheckAndOnBuild(t *testing.T) {
	plan := testPlan(t, `
from "scratch"
healthcheck interval="30s" timeout="3s" start-period="5s" start-interval="1s" retries=4 { exec "/bin/check" "--ready" }
onbuild { run "make generated" }
`)
	request, err := RequestFromPlan(plan, PlanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	wantHealthcheck := Healthcheck(imageconfig.Healthcheck{
		Test: []string{"CMD", "/bin/check", "--ready"}, Interval: 30 * time.Second, Timeout: 3 * time.Second,
		StartPeriod: 5 * time.Second, StartInterval: time.Second, Retries: 4,
	})
	want := []Operation{wantHealthcheck, OnBuild("RUN make generated")}
	if !reflect.DeepEqual(request.Operations, want) {
		t.Fatalf("operations = %#v, want %#v", request.Operations, want)
	}
}

func TestRequestFromPlanLowersLinkForCopyAndAdd(t *testing.T) {
	plan := testPlan(t, `
from "scratch"
copy "linked-copy" "/linked-copy" link="true"
copy "plain-copy" "/plain-copy" link="false"
add "linked-add" "/linked-add" link="true"
add "plain-add" "/plain-add" link="false"
`)
	request, err := RequestFromPlan(plan, PlanOptions{ContextDir: "/context"})
	if err != nil {
		t.Fatal(err)
	}
	if len(request.Operations) != 4 {
		t.Fatalf("operations = %#v, want four COPY/ADD operations", request.Operations)
	}
	want := []bool{true, false, true, false}
	got := []bool{
		request.Operations[0].(Copy).Link,
		request.Operations[1].(Copy).Link,
		request.Operations[2].(Add).Link,
		request.Operations[3].(Add).Link,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("lowered link values = %v, want %v", got, want)
	}
}

func TestRequestFromPlanRejectsMalformedCopyAndAddLink(t *testing.T) {
	for _, instruction := range []string{"copy", "add"} {
		t.Run(instruction, func(t *testing.T) {
			_, err := RequestFromPlan(testPlan(t, "from \"scratch\"\n"+instruction+" \"source\" \"/output\" link=\"sometimes\"\n"), PlanOptions{ContextDir: "/context"})
			if err == nil || !strings.Contains(err.Error(), strings.ToUpper(instruction)+" link must be true or false") {
				t.Fatalf("malformed %s link error = %v", instruction, err)
			}
		})
	}
}

func TestLowerGraphOperationsPassesLinkForStageCopy(t *testing.T) {
	const imageID = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	operation := planner.Operation{Instruction: definition.Instruction{
		Name: "copy", Arguments: []string{"/source", "/output"},
		Properties: map[string]string{"from": "source", "link": "true"},
	}}
	lowered, err := lowerGraphOperations(
		[]planner.Operation{operation},
		map[string]string{"source": "source-stage"},
		map[string]stageState{"source-stage": {storageImageID: imageID}},
		nil,
		[]string{"/bin/sh", "-c"},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	copied, ok := lowered[0].(copyFromImageOperation)
	if !ok || !copied.options.Link {
		t.Fatalf("stage-backed COPY = %#v, want Buildah Link enabled", lowered[0])
	}
}

func TestRequestFromPlanLowersParentsForCopyAndAdd(t *testing.T) {
	plan := testPlan(t, `
from "scratch"
copy "copy/./nested" "/copy/" parents="true"
add "add/./nested" "/add/" parents="true"
`)
	request, err := RequestFromPlan(plan, PlanOptions{ContextDir: "/context"})
	if err != nil {
		t.Fatal(err)
	}
	if len(request.Operations) != 2 {
		t.Fatalf("operations = %#v, want COPY and ADD", request.Operations)
	}
	copyOperation, copyOK := request.Operations[0].(Copy)
	addOperation, addOK := request.Operations[1].(Add)
	if !copyOK || !addOK || !copyOperation.Parents || !addOperation.Parents {
		t.Fatalf("parents operations = %#v, want enabled COPY and ADD", request.Operations)
	}
}

func TestRequestFromPlanLowersOrderedRunMounts(t *testing.T) {
	plan := testPlan(t, `
from "scratch"
run "true" {
 mount "bind" source="input" target="/input" readonly="true"
 mount "cache" target="/cache" sharing="locked"
 mount "tmpfs" target="/ram" size="32m"
}
`)
	resolverCalls := []int{}
	request, err := RequestFromPlan(plan, PlanOptions{ContextDir: "/context", CacheMountID: func(_ planner.Operation, mountIndex int) (string, error) {
		resolverCalls = append(resolverCalls, mountIndex)
		return "scoped-cache", nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	run := request.Operations[0].(Run)
	if !reflect.DeepEqual(resolverCalls, []int{1}) {
		t.Fatalf("cache resolver calls = %v", resolverCalls)
	}
	want := []RunMount{
		{Type: "bind", Properties: map[string]string{"source": "input", "target": "/input", "readonly": "true"}},
		{Type: "cache", Properties: map[string]string{"id": "scoped-cache", "target": "/cache", "sharing": "locked"}},
		{Type: "tmpfs", Properties: map[string]string{"target": "/ram", "size": "32m"}},
	}
	if !reflect.DeepEqual(run.Mounts, want) {
		t.Fatalf("RUN mounts = %#v, want %#v", run.Mounts, want)
	}
}

func TestRequestFromPlanRejectsUnboundAndAmbiguousRunMounts(t *testing.T) {
	tests := []struct{ name, mount, message string }{
		{"ssh invalid property", `mount "ssh" env="TOKEN"`, "not supported"},
		{"comma", `mount "tmpfs" target="/tmp,other"`, "unsafe or ambiguous"},
		{"context escape", `mount "bind" source="../secret" target="/secret"`, "escapes the build context"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			plan := testPlan(t, "from \"scratch\"\nrun \"true\" { "+test.mount+" }\n")
			request, err := RequestFromPlan(plan, PlanOptions{ContextDir: "/context"})
			if err == nil {
				fake := &recordingBuilder{}
				err = applyOperations(context.Background(), fake, "/context", request.Operations)
			}
			if err == nil || !strings.Contains(err.Error(), test.message) {
				t.Fatalf("want %q, got %v", test.message, err)
			}
		})
	}
}

func TestLowerRunMountsRejectsStageBackedSources(t *testing.T) {
	for _, mountType := range []string{"bind", "cache"} {
		t.Run(mountType, func(t *testing.T) {
			properties := map[string]string{"from": "build", "target": "/input"}
			if mountType == "cache" {
				properties["id"] = "cache"
			}
			operation := planner.Operation{Instruction: definition.Instruction{
				Name: "run", Arguments: []string{"true"}, Children: []definition.Instruction{{
					Name: "mount", Arguments: []string{mountType}, Properties: properties,
				}},
			}}
			_, _, err := lowerOperation(operation, []string{"/bin/sh", "-c"})
			if err == nil || !strings.Contains(err.Error(), "from stages or images") {
				t.Fatalf("stage-backed %s mount error = %v", mountType, err)
			}
		})
	}
}

func TestRequestFromPlanPreservesNoNetworkRun(t *testing.T) {
	plan := testPlan(t, "from \"scratch\"\nrun \"true\" network=\"none\"\n")
	request, err := RequestFromPlan(plan, PlanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := request.Operations[0].(Run).Network; got != "none" {
		t.Fatalf("RUN network policy = %q, want none", got)
	}
}

func TestRequestFromPlanBuildsScratchCopy(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH=1 for a live rootless Buildah build")
	}
	root := t.TempDir()
	contextDir := filepath.Join(root, "context")
	if err := os.Mkdir(contextDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(contextDir, "proof"), []byte("planned\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	request, err := RequestFromPlan(testPlan(t, `
from "scratch"
copy "proof" "/proof"
env COOPR_PATH="planned"
cmd { exec "/proof" }
`), PlanOptions{
		Store: StoreOptions{
			RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs",
		},
		ContextDir: contextDir, Isolation: "rootless",
		Output: Output{Path: filepath.Join(root, "layout"), Reference: "planned-scratch"},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	result, err := Build(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if result.ManifestDigest == "" {
		t.Fatal("planned build returned no manifest digest")
	}
}

func testPlan(t *testing.T, source string) *planner.Plan {
	t.Helper()
	parsed, err := definition.Parse(strings.NewReader(source))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := planner.Create(parsed, planner.Options{Mode: planner.Build, Platform: runtime.GOOS + "/" + runtime.GOARCH})
	if err != nil {
		t.Fatal(err)
	}
	return plan
}
