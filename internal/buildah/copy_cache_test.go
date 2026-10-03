package buildah

import (
	"testing"

	"coopr/internal/definition"
	"coopr/internal/imageconfig"
	"coopr/internal/planner"
	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestInstructionCacheKeySupportsFilesystemInstructions(t *testing.T) {
	base := instructionCacheInput{
		ParentImageID: "parent", Logical: imageconfig.New(),
		Platform:  v1.Platform{OS: "linux", Architecture: "amd64"},
		Isolation: "rootless", Runtime: "/usr/bin/crun@sha256:runtime", Format: "oci",
	}
	tests := []struct {
		name      string
		operation planner.Operation
		digest    digest.Digest
		cacheable bool
	}{
		{name: "copy", operation: cacheTestOperation("copy", "src", "/dst"), digest: digest.FromString("copy input"), cacheable: true},
		{name: "add", operation: cacheTestOperation("add", "archive.tar", "/dst"), digest: digest.FromString("add input"), cacheable: true},
		{name: "run", operation: cacheTestOperation("run", "printf ready"), cacheable: true},
		{name: "workdir", operation: cacheTestOperation("workdir", "/workspace"), cacheable: true},
		{name: "env", operation: cacheTestOperation("env", "NAME", "value")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := base
			input.Operation = test.operation
			input.InputDigest = test.digest
			key, cacheable, err := instructionCacheKey(input)
			if err != nil {
				t.Fatal(err)
			}
			if cacheable != test.cacheable {
				t.Fatalf("cacheable = %v, want %v", cacheable, test.cacheable)
			}
			if test.cacheable && key == "" {
				t.Fatal("cacheable instruction returned an empty key")
			}
			if !test.cacheable && key != "" {
				t.Fatalf("uncacheable instruction returned key %s", key)
			}
		})
	}
}

func TestInstructionCacheKeyIncludesCopyAddInputDigest(t *testing.T) {
	for _, name := range []string{"copy", "add"} {
		t.Run(name, func(t *testing.T) {
			input := instructionCacheInput{
				ParentImageID: "parent", Logical: imageconfig.New(),
				Operation: cacheTestOperation(name, "source", "/destination"),
				Platform:  v1.Platform{OS: "linux", Architecture: "amd64"},
				Isolation: "rootless", Format: "oci", InputDigest: digest.FromString("first input"),
			}
			first, cacheable, err := instructionCacheKey(input)
			if err != nil || !cacheable {
				t.Fatalf("first key = %q, cacheable = %v, error = %v", first, cacheable, err)
			}
			input.InputDigest = digest.FromString("second input")
			second, cacheable, err := instructionCacheKey(input)
			if err != nil || !cacheable {
				t.Fatalf("second key = %q, cacheable = %v, error = %v", second, cacheable, err)
			}
			if first == second {
				t.Fatal("input digest did not change the instruction cache key")
			}
		})
	}
}

func cacheTestOperation(name string, arguments ...string) planner.Operation {
	return planner.Operation{Instruction: definition.Instruction{Name: name, Arguments: arguments}}
}
