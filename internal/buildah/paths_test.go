package buildah

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestDefaultStoreOptionsUseEffectiveNativeConfiguration(t *testing.T) {
	root := t.TempDir()
	data := filepath.Join(root, "data")
	runtimeDir := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", filepath.Join(root, "cache"))
	t.Setenv("XDG_DATA_HOME", data)
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	t.Setenv("STORAGE_DRIVER", "")
	t.Setenv("STORAGE_OPTS", "")
	// An explicitly empty STORAGE_OPTS disables configured driver options.
	if err := os.Unsetenv("STORAGE_OPTS"); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, "storage.conf")
	t.Setenv("CONTAINERS_STORAGE_CONF", configPath)
	for _, helper := range []string{"first-helper", "second-helper"} {
		t.Run(helper, func(t *testing.T) {
			mountProgram := filepath.Join(root, helper)
			config := fmt.Sprintf(`[storage]
driver = "overlay"
driver_priority = ["overlay", "vfs"]
graphroot = %q
runroot = %q
imagestore = %q
transient_store = true
[storage.options.overlay]
mount_program = %q
`, filepath.Join(root, "foreign-graph"), filepath.Join(root, "foreign-run"), filepath.Join(root, "foreign-images"), mountProgram)
			if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
				t.Fatal(err)
			}
			got, err := DefaultStoreOptions()
			if err != nil {
				t.Fatal(err)
			}
			native := NativeStoreOptions(got)
			if native.GraphRoot != filepath.Join(root, "foreign-graph") || native.RunRoot != filepath.Join(root, "foreign-run") {
				t.Fatalf("native configured roots were replaced: %+v", native)
			}
			if native.ImageStore != filepath.Join(root, "foreign-images") || !native.TransientStore {
				t.Fatalf("configured native settings were lost: %+v", got)
			}
			if native.GraphDriverName != "overlay" || !slices.Equal(native.GraphDriverOptions, []string{"overlay.mount_program=" + mountProgram}) {
				t.Fatalf("configured storage driver/options = %q %v, want overlay with current mount helper %q", native.GraphDriverName, native.GraphDriverOptions, mountProgram)
			}
			if !slices.Equal(native.GraphDriverPriority, []string{"overlay", "vfs"}) {
				t.Fatalf("configured driver priority = %v", native.GraphDriverPriority)
			}
		})
	}
}

func TestCanonicalStorePathDetectsSymlinkedOverlap(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "real")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(real, alias); err != nil {
		t.Fatal(err)
	}
	runRoot, err := canonicalStorePath(filepath.Join(alias, "run"))
	if err != nil {
		t.Fatal(err)
	}
	if runRoot != filepath.Join(real, "run") {
		t.Fatalf("canonical path = %q, want %q", runRoot, filepath.Join(real, "run"))
	}
	graphRoot, err := canonicalStorePath(filepath.Join(real, "run", "graph"))
	if err != nil {
		t.Fatal(err)
	}
	if !pathsOverlap(runRoot, graphRoot) {
		t.Fatalf("symlinked store roots do not overlap: %q, %q", runRoot, graphRoot)
	}
	if pathsOverlap(filepath.Join(real, "run"), filepath.Join(real, "graph")) {
		t.Fatal("sibling store roots overlap")
	}
}
