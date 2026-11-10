package buildah

import (
	"context"
	"github.com/openshift/imagebuilder"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/strio-dev/coopr/internal/definition"
	"github.com/strio-dev/coopr/internal/planner"
)

func TestMetadataInstructionsPreserveAuthoredOrder(t *testing.T) {
	operations, err := lowerGraphOperations((testPlan(t, `
from "scratch" {
    expose "8080" "8443/tcp"
    volume " /var/lib/data " "/var/cache"
    maintainer "A B <a@example.com>"
    expose "9090/udp"
}
`)).Stages[0].Operations, nil, nil, nil, []string{"/bin/sh", "-c"}, nil)
	if err != nil {
		t.Fatal(err)
	}

	wantOperations := []Operation{
		Expose("8080/tcp"), Expose("8443/tcp"),
		Volume("/var/lib/data"), Volume("/var/cache"),
		Maintainer("A B <a@example.com>"), Expose("9090/udp"),
	}
	if !reflect.DeepEqual(operations, wantOperations) {
		t.Fatalf("operations = %#v, want %#v", operations, wantOperations)
	}

	fake := &recordingBuilder{}
	for _, operation := range operations {
		if err := applyOperationWithContextContext(context.Background(), fake, "", nil, operation); err != nil {
			t.Fatal(err)
		}
	}
	wantEvents := []string{
		"expose:8080/tcp", "expose:8443/tcp",
		"volume:/var/lib/data", "volume:/var/cache",
		"maintainer:A B <a@example.com>", "expose:9090/udp",
	}
	if !reflect.DeepEqual(fake.events, wantEvents) {
		t.Fatalf("events = %#v, want %#v", fake.events, wantEvents)
	}
}

func TestExposePreservesExpandedTokenWithNativeSemantics(t *testing.T) {
	operations, err := lowerGraphOperations((testPlan(t, `
arg "ports" "80 81/UDP 82-83/sCtP"
from "scratch" {
    arg "ports"
    expose "$ports" "127.0.0.1:9000:84"
}
`)).Stages[0].Operations, nil, nil, nil, []string{"/bin/sh", "-c"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	native := imagebuilder.NewBuilder(nil)
	if err := native.Run(&imagebuilder.Step{Command: "expose", Args: []string{"80 81/UDP 82-83/sCtP", "127.0.0.1:9000:84"}}, imagebuilder.NoopExecutor, true); err != nil {
		t.Fatal(err)
	}
	ports := make([]string, 0, len(native.RunConfig.ExposedPorts))
	for port := range native.RunConfig.ExposedPorts {
		ports = append(ports, string(port))
	}
	slices.Sort(ports)
	want := make([]Operation, 0, len(ports))
	wantEvents := make([]string, 0, len(ports))
	for _, port := range ports {
		want = append(want, Expose(port))
		wantEvents = append(wantEvents, "expose:"+port)
	}
	if !reflect.DeepEqual(operations, want) {
		t.Fatalf("operations = %#v, want %#v", operations, want)
	}

	fake := &recordingBuilder{}
	for _, operation := range operations {
		if err := applyOperationWithContextContext(context.Background(), fake, "", nil, operation); err != nil {
			t.Fatal(err)
		}
	}
	if !reflect.DeepEqual(fake.events, wantEvents) {
		t.Fatalf("events = %#v, want %#v", fake.events, wantEvents)
	}
}

func TestStopSignalValidatesBeforeLowering(t *testing.T) {
	for _, value := range []string{"SIGTERM", "term", "15"} {
		t.Run("valid "+value, func(t *testing.T) {
			instruction := definition.Instruction{Name: "stopsignal", Arguments: []string{value}}
			operations, _, err := lowerOperationWithCacheMountIDs(planner.Operation{Instruction: instruction}, []string{"/bin/sh", "-c"}, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(operations, []Operation{StopSignal(value)}) {
				t.Fatalf("operations = %#v", operations)
			}
		})
	}

	for _, value := range []string{"", "0", "SIGBOGUS"} {
		t.Run("invalid "+value, func(t *testing.T) {
			instruction := definition.Instruction{Name: "stopsignal", Arguments: []string{value}}
			_, _, err := lowerOperationWithCacheMountIDs(planner.Operation{Instruction: instruction}, []string{"/bin/sh", "-c"}, nil, nil)
			if err == nil || !strings.Contains(err.Error(), "invalid signal") {
				t.Fatalf("invalid STOPSIGNAL %q error = %v", value, err)
			}
		})
	}
}

func TestLowerMetadataInstructionsRejectsUnsupportedShapes(t *testing.T) {
	tests := []struct {
		name        string
		instruction definition.Instruction
		message     string
	}{
		{"expose requires a port", definition.Instruction{Name: "expose"}, "at least one port"},
		{"volume requires a path", definition.Instruction{Name: "volume"}, "at least one path"},
		{"volume path is nonempty", definition.Instruction{Name: "volume", Arguments: []string{"  "}}, "is invalid"},
		{"maintainer requires one value", definition.Instruction{Name: "maintainer"}, "requires 1 arguments"},
		{"maintainer is single line", definition.Instruction{Name: "maintainer", Arguments: []string{"A\nB"}}, "single-line"},
		{"metadata rejects properties", definition.Instruction{Name: "volume", Arguments: []string{"/data"}, Properties: map[string]string{"mode": "0755"}}, "form, properties, and children"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, _, err := lowerOperationWithCacheMountIDs(planner.Operation{Instruction: test.instruction}, []string{"/bin/sh", "-c"}, nil, nil)
			if err == nil || !strings.Contains(err.Error(), test.message) {
				t.Fatalf("want %q, got %v", test.message, err)
			}
		})
	}
}
