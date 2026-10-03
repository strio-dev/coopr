package buildah

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"coopr/internal/definition"
	"coopr/internal/planner"
)

func TestMetadataInstructionsPreserveAuthoredOrder(t *testing.T) {
	request, err := RequestFromPlan(testPlan(t, `
from "scratch"
expose "8080" "8443/tcp"
volume " /var/lib/data " "/var/cache"
maintainer "A B <a@example.com>"
expose "9090/udp"
`), PlanOptions{})
	if err != nil {
		t.Fatal(err)
	}

	wantOperations := []Operation{
		Expose("8080/tcp"), Expose("8443/tcp"),
		Volume("/var/lib/data"), Volume("/var/cache"),
		Maintainer("A B <a@example.com>"), Expose("9090/udp"),
	}
	if !reflect.DeepEqual(request.Operations, wantOperations) {
		t.Fatalf("operations = %#v, want %#v", request.Operations, wantOperations)
	}

	fake := &recordingBuilder{}
	if err := applyOperations(context.Background(), fake, "", request.Operations); err != nil {
		t.Fatal(err)
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

func TestExposeSplitsExpandedValuesAndNormalizesPorts(t *testing.T) {
	request, err := RequestFromPlan(testPlan(t, `
arg "ports" "80 81/UDP 82-83/sCtP"
from "scratch"
arg "ports"
expose "$ports" "127.0.0.1:9000:84"
`), PlanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	want := []Operation{
		Expose("80/tcp"), Expose("81/udp"),
		Expose("82/sctp"), Expose("83/sctp"), Expose("84/tcp"),
	}
	if !reflect.DeepEqual(request.Operations, want) {
		t.Fatalf("operations = %#v, want %#v", request.Operations, want)
	}

	fake := &recordingBuilder{}
	if err := applyOperations(context.Background(), fake, "", request.Operations); err != nil {
		t.Fatal(err)
	}
	wantEvents := []string{"expose:80/tcp", "expose:81/udp", "expose:82/sctp", "expose:83/sctp", "expose:84/tcp"}
	if !reflect.DeepEqual(fake.events, wantEvents) {
		t.Fatalf("events = %#v, want %#v", fake.events, wantEvents)
	}
}

func TestExposeRejectsInvalidPortSpecifications(t *testing.T) {
	tests := []struct {
		name, port, message string
	}{
		{"unknown protocol", "80/http", "invalid proto"},
		{"missing port", "/tcp", "no port specified"},
		{"nonnumeric port", "http", "invalid containerPort"},
		{"port above maximum", "65536", "invalid containerPort"},
		{"descending range", "90-80", "invalid containerPort"},
		{"range width mismatch", "8000-8002:80-81", "invalid ranges specified"},
		{"invalid host IP", "not-an-ip:8000:80", "invalid IP address"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			instruction := definition.Instruction{Name: "expose", Arguments: []string{test.port}}
			_, _, err := lowerOperation(planner.Operation{Instruction: instruction}, []string{"/bin/sh", "-c"})
			if err == nil || !strings.Contains(err.Error(), test.message) {
				t.Fatalf("want %q, got %v", test.message, err)
			}
		})
	}
}

func TestStopSignalValidatesBeforeLowering(t *testing.T) {
	for _, value := range []string{"SIGTERM", "term", "15"} {
		t.Run("valid "+value, func(t *testing.T) {
			instruction := definition.Instruction{Name: "stopsignal", Arguments: []string{value}}
			operations, _, err := lowerOperation(planner.Operation{Instruction: instruction}, []string{"/bin/sh", "-c"})
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
			_, _, err := lowerOperation(planner.Operation{Instruction: instruction}, []string{"/bin/sh", "-c"})
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
			_, _, err := lowerOperation(planner.Operation{Instruction: test.instruction}, []string{"/bin/sh", "-c"})
			if err == nil || !strings.Contains(err.Error(), test.message) {
				t.Fatalf("want %q, got %v", test.message, err)
			}
		})
	}
}
