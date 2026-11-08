package buildah

import (
	"testing"

	digest "github.com/opencontainers/go-digest"
)

func TestInstructionGraphSeparatesBackendSemantics(t *testing.T) {
	key := digest.FromString("same operation and logical parents")
	type backend struct {
		modules, driver, runtime, format string
		options                          []string
	}
	baseline := backend{modules: "storage@1/image@1", driver: "vfs", runtime: "crun-v1", format: "oci"}
	derive := func(value backend) digest.Digest {
		t.Helper()
		namespace, err := componentCacheExecutorSemantics("buildah-v1", value.modules, "rootless", value.driver, value.options, value.runtime, value.format)
		if err != nil {
			t.Fatal(err)
		}
		return instructionGraphNamespacedKey(key, namespace)
	}
	want := derive(baseline)
	if derive(baseline) != want {
		t.Fatal("backend namespace is unstable")
	}
	for _, change := range []struct {
		name  string
		apply func(*backend)
	}{
		{"modules", func(b *backend) { b.modules = "storage@2/image@1" }},
		{"driver", func(b *backend) { b.driver = "overlay" }},
		{"options", func(b *backend) { b.options = []string{"overlay.mountopt=nodev"} }},
		{"runtime", func(b *backend) { b.runtime = "crun-v2" }},
		{"format", func(b *backend) { b.format = "docker" }},
	} {
		t.Run(change.name, func(t *testing.T) {
			value := baseline
			change.apply(&value)
			if derive(value) == want {
				t.Fatal("incompatible backend reused a logical graph key")
			}
		})
	}
}
