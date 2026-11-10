package buildah

import (
	"context"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/strio-dev/coopr/internal/definition"
	"github.com/strio-dev/coopr/internal/imageconfig"
	"github.com/strio-dev/coopr/internal/planner"
)

func TestLowerGraphOperationsPreservesInstructionAndCommandForms(t *testing.T) {
	plan := testPlan(t, `
arg "mode" "test"
from "scratch" {
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
}
`)
	operations, err := lowerGraphOperations((plan).Stages[0].Operations, nil, nil, nil, []string{"/bin/sh", "-c"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(operations) != 12 {
		t.Fatalf("operations = %+v", operations)
	}
	wantTypes := []any{
		Env{}, Env{}, Copy{}, Shell{}, Run{}, Run{}, Label{}, WorkDir(""), User(""), Cmd{}, Entrypoint{}, StopSignal(""),
	}
	for index, want := range wantTypes {
		if reflect.TypeOf(operations[index]) != reflect.TypeOf(want) {
			t.Fatalf("operation %d type = %T, want %T", index+1, operations[index], want)
		}
	}
	copyOperation := operations[2].(Copy)
	if !reflect.DeepEqual(copyOperation.Sources, []string{"one/./nested", "two/./nested"}) || !reflect.DeepEqual(copyOperation.Excludes, []string{"*.tmp"}) || !copyOperation.Parents {
		t.Fatalf("COPY = %+v", copyOperation)
	}
	shellRun := operations[4].(Run)
	if !reflect.DeepEqual(shellRun.Command, []string{"/bin/bash", "-ceu", `printf '%s' "$mode"`}) || !reflect.DeepEqual(shellRun.Env, []string{"mode=test"}) {
		t.Fatalf("shell RUN = %+v", shellRun)
	}
	execRun := operations[5].(Run)
	if !reflect.DeepEqual(execRun.Command, []string{"/bin/echo", "ok"}) {
		t.Fatalf("exec RUN = %+v", execRun)
	}
	if got := operations[9].(Cmd); !reflect.DeepEqual(got, Cmd{"/bin/bash", "-ceu", "serve"}) {
		t.Fatalf("shell CMD = %#v", got)
	}
}

func TestLowerGraphOperationsDoesNotDuplicateGlobalMounts(t *testing.T) {
	mounts, err := ParseTransientRunMounts([]string{"type=bind,target=/global"})
	if err != nil {
		t.Fatal(err)
	}
	def, err := definition.Parse(strings.NewReader(`from "scratch" {
    run "true" { mount "bind" target="/authored" }
}
`))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := planner.Create(def, planner.Options{Mode: planner.Build, Platform: runtime.GOOS + "/" + runtime.GOARCH, TransientRunMounts: TransientMountInstructions(mounts)})
	if err != nil {
		t.Fatal(err)
	}
	operations, err := lowerGraphOperations((plan).Stages[0].Operations, nil, nil, nil, []string{"/bin/sh", "-c"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	run := operations[0].(Run)
	counts := map[string]int{}
	for _, mount := range run.Mounts {
		counts[mount.Properties["target"]]++
	}
	if counts["/global"] != 1 || counts["/authored"] != 1 || len(counts) != 2 {
		t.Fatalf("effective mount counts=%v", counts)
	}

}

func TestLowerGraphOperationsPreservesAddGitAndUnpackOptions(t *testing.T) {
	plan := testPlan(t, `
from "scratch" {
    add "https://example.invalid/repository.git#main" "/source/" keep-git-dir="true" checksum="abcdef"
    add "archive.tar" "/archive/" unpack="false"
}
`)
	operations, err := lowerGraphOperations((plan).Stages[0].Operations, nil, nil, nil, []string{"/bin/sh", "-c"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	gitAdd := operations[0].(Add)
	if gitAdd.KeepGitDir == nil || !*gitAdd.KeepGitDir || gitAdd.Checksum != "abcdef" || gitAdd.Unpack != nil {
		t.Fatalf("Git ADD = %+v", gitAdd)
	}
	archiveAdd := operations[1].(Add)
	if archiveAdd.Unpack == nil || *archiveAdd.Unpack || archiveAdd.KeepGitDir != nil {
		t.Fatalf("archive ADD = %+v", archiveAdd)
	}
}

func TestLowerGraphOperationsRejectsInvalidAddBooleanOptions(t *testing.T) {
	for _, property := range []string{"keep-git-dir", "unpack"} {
		t.Run(property, func(t *testing.T) {
			_, err := lowerGraphOperations((testPlan(t, `from "scratch" {
    add "source" "/source" `+property+`="sometimes"
}
`)).Stages[0].Operations, nil, nil, nil, []string{"/bin/sh", "-c"}, nil)
			if err == nil || !strings.Contains(err.Error(), property+" must be true or false") {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestLowerGraphOperationsRejectsUnsupportedSemantics(t *testing.T) {
	tests := []struct {
		name, source, message string
	}{
		{"run mount without cache scope", `from "scratch" {
    run "true" { mount "cache" target="/cache" }
}
`, "requires executor scope"},
		{"copy invalid parents", `from "scratch" {
    copy "a" "/a" parents="sometimes"
}
`, "parents must be true or false"},
		{"copy remote URL", `from "scratch" {
    copy "https://example.invalid/file" "/file"
}
`, "COPY source must be local"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := lowerGraphOperations((testPlan(t, test.source)).Stages[0].Operations, nil, nil, nil, []string{"/bin/sh", "-c"}, nil)
			if err == nil || !strings.Contains(err.Error(), test.message) {
				t.Fatalf("want %q, got %v", test.message, err)
			}
		})
	}
}

func TestLowerOperationRejectsUnknownInstruction(t *testing.T) {
	_, _, err := lowerOperationWithCacheMountIDs(planner.Operation{Instruction: definition.Instruction{Name: "frobnicate", Arguments: []string{"value"}}}, nil, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "instruction is not supported") {
		t.Fatalf("unknown operation error = %v", err)
	}
}

func TestLowerGraphOperationsLowersRunHostNetwork(t *testing.T) {
	operations, err := lowerGraphOperations((testPlan(t, `from "scratch" {
    run "true" network="host"
}
`)).Stages[0].Operations, nil, nil, nil, []string{"/bin/sh", "-c"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	run := operations[0].(Run)
	if run.Network != "host" {
		t.Fatalf("lowered RUN = %#v", run)
	}
}

func TestLowerRunRejectsRetiredSecurityProperty(t *testing.T) {
	for _, mode := range []string{"sandbox", "insecure"} {
		instruction := definition.Instruction{Name: "run", Arguments: []string{"true"}, Properties: map[string]string{"security": mode}}
		if _, _, err := lowerOperationWithCacheMountIDs(planner.Operation{Instruction: instruction}, nil, nil, nil); err == nil {
			t.Fatalf("retired RUN security property %q was lowered", mode)
		}
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
	operations, err := lowerGraphOperations((testPlan(t, `
from "scratch" {
    run """
        #!/bin/sh
        true
        """
}
`)).Stages[0].Operations, nil, nil, nil, []string{"/bin/sh", "-c"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	copyOperation, copyOK := copyOperations[0].(Copy)
	runOperation, runOK := operations[0].(Run)
	if !copyOK || !reflect.DeepEqual(copyOperation.InlineFiles, []definition.InlineFile{{Path: "one", Data: "first"}, {Path: "two", Data: "second"}}) {
		t.Fatalf("lowered imported COPY = %#v", copyOperations[0])
	}
	if !runOK || !reflect.DeepEqual(runOperation.Command, []string{"/run/coopr-heredoc/script"}) || len(runOperation.InlineFiles) != 1 {
		t.Fatalf("lowered shebang RUN = %#v", operations[0])
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

func TestLowerGraphOperationsLowersRepeatedRunDevicesAlongsideMounts(t *testing.T) {
	operations, err := lowerGraphOperations((testPlan(t, `
from "scratch" {
    run "true" {
      device "vendor.example/device=one,required"
      mount "tmpfs" target="/tmp"
      device "accelerator,required=false"
    }
}
`)).Stages[0].Operations, nil, nil, nil, []string{"/bin/sh", "-c"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	run := operations[0].(Run)
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

func TestLowerGraphOperationsRejectsInvalidRunDeviceOption(t *testing.T) {
	_, err := lowerGraphOperations((testPlan(t, `from "scratch" {
    run "true" { device "name=vendor.example/device=one,required=sometimes" }
}
`)).Stages[0].Operations, nil, nil, nil, []string{"/bin/sh", "-c"}, nil)
	if err == nil || !strings.Contains(err.Error(), "invalid value for required") {
		t.Fatalf("invalid RUN device error = %v", err)
	}
}

func TestLowerGraphOperationsLowersHealthcheckAndOnBuild(t *testing.T) {
	plan := testPlan(t, `
from "scratch" {
    healthcheck interval="30s" timeout="3s" start-period="5s" start-interval="1s" retries=4 { exec "/bin/check" "--ready" }
    onbuild { run "make generated" }
}
`)
	operations, err := lowerGraphOperations((plan).Stages[0].Operations, nil, nil, nil, []string{"/bin/sh", "-c"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	wantHealthcheck := Healthcheck(imageconfig.Healthcheck{
		Test: []string{"CMD", "/bin/check", "--ready"}, Interval: 30 * time.Second, Timeout: 3 * time.Second,
		StartPeriod: 5 * time.Second, StartInterval: time.Second, Retries: 4,
	})
	want := []Operation{wantHealthcheck, OnBuild("RUN make generated")}
	if !reflect.DeepEqual(operations, want) {
		t.Fatalf("operations = %#v, want %#v", operations, want)
	}
}

func TestLowerGraphOperationsLowersLinkForCopyAndAdd(t *testing.T) {
	plan := testPlan(t, `
from "scratch" {
    copy "linked-copy" "/linked-copy" link="true"
    copy "plain-copy" "/plain-copy" link="false"
    add "linked-add" "/linked-add" link="true"
    add "plain-add" "/plain-add" link="false"
}
`)
	operations, err := lowerGraphOperations((plan).Stages[0].Operations, nil, nil, nil, []string{"/bin/sh", "-c"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(operations) != 4 {
		t.Fatalf("operations = %#v, want four COPY/ADD operations", operations)
	}
	want := []bool{true, false, true, false}
	got := []bool{
		operations[0].(Copy).Link,
		operations[1].(Copy).Link,
		operations[2].(Add).Link,
		operations[3].(Add).Link,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("lowered link values = %v, want %v", got, want)
	}
}

func TestLowerGraphOperationsRejectsMalformedCopyAndAddLink(t *testing.T) {
	for _, instruction := range []string{"copy", "add"} {
		t.Run(instruction, func(t *testing.T) {
			_, err := lowerGraphOperations((testPlan(t, `from "scratch" {
    `+instruction+` "source" "/output" link="sometimes"
}
`)).Stages[0].Operations, nil, nil, nil, []string{"/bin/sh", "-c"}, nil)
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

func TestLowerGraphOperationsLowersParentsForCopyAndAdd(t *testing.T) {
	plan := testPlan(t, `
from "scratch" {
    copy "copy/./nested" "/copy/" parents="true"
    add "add/./nested" "/add/" parents="true"
}
`)
	operations, err := lowerGraphOperations((plan).Stages[0].Operations, nil, nil, nil, []string{"/bin/sh", "-c"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(operations) != 2 {
		t.Fatalf("operations = %#v, want COPY and ADD", operations)
	}
	copyOperation, copyOK := operations[0].(Copy)
	addOperation, addOK := operations[1].(Add)
	if !copyOK || !addOK || !copyOperation.Parents || !addOperation.Parents {
		t.Fatalf("parents operations = %#v, want enabled COPY and ADD", operations)
	}
}

func TestLowerGraphOperationsLowersOrderedRunMounts(t *testing.T) {
	plan := testPlan(t, `
from "scratch" {
    run "true" {
     mount "bind" source="input" target="/input" readonly="true"
     mount "cache" target="/cache" sharing="locked"
     mount "tmpfs" target="/ram" size="32m"
    }
}
`)
	resolverCalls := []int{}
	operations, err := lowerGraphOperations((plan).Stages[0].Operations, nil, nil, nil, []string{"/bin/sh", "-c"}, func(_ planner.Operation, mountIndex int) (string, error) {
		resolverCalls = append(resolverCalls, mountIndex)
		return "scoped-cache", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	run := operations[0].(Run)
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

func TestLowerGraphOperationsRejectsUnboundAndAmbiguousRunMounts(t *testing.T) {
	tests := []struct{ name, mount, message string }{
		{"comma", `mount "tmpfs" target="/tmp,other"`, "unsafe or ambiguous"},
		{"context escape", `mount "bind" source="../secret" target="/secret"`, "escapes the build context"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			plan := testPlan(t, `from "scratch" {
    run "true" { `+test.mount+` }
}
`)
			operations, err := lowerGraphOperations((plan).Stages[0].Operations, nil, nil, nil, []string{"/bin/sh", "-c"}, nil)
			if err == nil {
				fake := &recordingBuilder{}
				err = applyOperationWithContextContext(context.Background(), fake, "/context", nil, operations[0])
			}
			if err == nil || !strings.Contains(err.Error(), test.message) {
				t.Fatalf("want %q, got %v", test.message, err)
			}
		})
	}
}

func TestSerializeRunMountRejectsUnsupportedProperty(t *testing.T) {
	_, err := serializeRunMount(RunMount{Type: "ssh", Properties: map[string]string{"env": "TOKEN"}}, "")
	if err == nil || !strings.Contains(err.Error(), `ssh mount property "env" is not supported`) {
		t.Fatalf("invalid SSH mount property error = %v", err)
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
			_, _, err := lowerOperationWithCacheMountIDs(operation, []string{"/bin/sh", "-c"}, nil, nil)
			if err == nil || !strings.Contains(err.Error(), "from stages or images") {
				t.Fatalf("stage-backed %s mount error = %v", mountType, err)
			}
		})
	}
}

func TestLowerGraphOperationsPreservesNoNetworkRun(t *testing.T) {
	plan := testPlan(t, `from "scratch" {
    run "true" network="none"
}
`)
	operations, err := lowerGraphOperations((plan).Stages[0].Operations, nil, nil, nil, []string{"/bin/sh", "-c"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := operations[0].(Run).Network; got != "none" {
		t.Fatalf("RUN network policy = %q, want none", got)
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
