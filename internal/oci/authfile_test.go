package oci

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestMaterializeRegistryAuthFilePreservesOrdinaryPaths(t *testing.T) {
	missingDefault := filepath.Join(t.TempDir(), "missing-auth.json")
	t.Setenv("REGISTRY_AUTH_FILE", missingDefault)
	for _, input := range []string{"", "relative-auth.json", missingDefault} {
		path, owned, err := MaterializeRegistryAuthFile(input)
		if err != nil || owned || path != input {
			t.Fatalf("ordinary authfile %q became %q, owned=%t, error=%v", input, path, owned, err)
		}
	}
}

func TestMaterializeRegistryAuthFileMakesDescriptorsReusable(t *testing.T) {
	for _, implicit := range []bool{false, true} {
		t.Run(fmt.Sprintf("implicit=%t", implicit), func(t *testing.T) {
			file, err := os.CreateTemp(t.TempDir(), "input-")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = file.Close() }()
			const credentials = `{"auths":{}}`
			if _, err := file.WriteString(credentials); err != nil {
				t.Fatal(err)
			}
			input := fmt.Sprintf("/dev/fd/%d", file.Fd())
			if implicit {
				t.Setenv("REGISTRY_AUTH_FILE", input)
				input = ""
			}
			path, owned, err := MaterializeRegistryAuthFile(input)
			if err != nil || !owned || path == "" || path == input {
				t.Fatalf("descriptor authfile = %q, owned=%t, error=%v", path, owned, err)
			}
			t.Cleanup(func() { _ = os.Remove(path) })
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(path)
			if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
				t.Fatalf("materialized authfile metadata = %v, error=%v", info, err)
			}
			for range 2 {
				data, err := os.ReadFile(path)
				if err != nil || string(data) != credentials {
					t.Fatalf("reusable authfile = %q, error=%v", data, err)
				}
			}
		})
	}
}

func TestMaterializeRegistryAuthFileRejectsNativeMirroringFailure(t *testing.T) {
	path, owned, err := MaterializeRegistryAuthFile("/dev/fd/2147483647")
	if err == nil || owned || path != "" {
		t.Fatalf("invalid descriptor = %q, owned=%t, error=%v", path, owned, err)
	}
}
