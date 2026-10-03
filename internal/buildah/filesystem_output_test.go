package buildah

import (
	"go.podman.io/buildah/define"
	"testing"
)

func TestParseFilesystemOutput(t *testing.T) {
	for _, test := range []struct{ value, kind, path string }{
		{"", "", ""}, {"out", "local", "out"}, {"-", "tar", "-"},
		{"type=local,dest=out", "local", "out"}, {"type=tar,dest=out.tar", "tar", "out.tar"},
		{"type=tar,dest=-", "tar", "-"}, {"type=local,\"dest=with,comma\"", "local", "with,comma"},
	} {
		output, err := ParseFilesystemOutput(test.value)
		if err != nil || output.Type != test.kind || output.Path != test.path {
			t.Fatalf("parse %q = %+v, %v", test.value, output, err)
		}
	}
	for _, value := range []string{"type=local", "type=tar", "dest=out", "type=local,dest=-", "type=oci,dest=out", "type=tar,dest=out,dest=again", "type=tar,type=local,dest=out", "type=tar,dest=", "type=tar,dest=out,unknown=x"} {
		if output, err := ParseFilesystemOutput(value); err == nil {
			t.Errorf("invalid output %q accepted: %+v", value, output)
		}
	}
}

func TestFinalizationWorkerBoundaryValidation(t *testing.T) {
	for _, output := range []Output{
		{Squash: true, SquashAll: true},
		{Filesystem: FilesystemOutput{Type: "oci", Path: "/out"}},
		{Filesystem: FilesystemOutput{Type: "tar", Path: "relative"}},
		{SBOM: []define.SBOMScanOptions{{Image: "scanner", Commands: []string{"scan"}}}},
		{SBOM: []define.SBOMScanOptions{{Image: "scanner", Commands: []string{"scan"}, SBOMOutput: "relative", MergeStrategy: define.SBOMMergeStrategyCat}}},
	} {
		if err := validateFinalizationOutput(output); err == nil {
			t.Fatalf("invalid worker output accepted: %+v", output)
		}
	}
}
