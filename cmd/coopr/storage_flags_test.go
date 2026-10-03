package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestGlobalStorageFlagsSelectOneCanonicalGraph(t *testing.T) {
	rootDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(rootDir, "config"))
	root := newRootCommand()
	var captured bool
	probe := &cobra.Command{Use: "storage-probe", RunE: func(cmd *cobra.Command, _ []string) error {
		store, catalog, err := commandStorage(cmd)
		if err != nil {
			return err
		}
		if store.GraphRoot != filepath.Join(rootDir, "custom-native") || store.RunRoot != filepath.Join(rootDir, "runtime") || store.GraphDriverName != "vfs" || !store.TransientStore || store.ImageStore != filepath.Join(rootDir, "image-bytes") {
			t.Fatalf("store=%+v", store)
		}
		wantCatalog, err := selectedStoreCatalog(store)
		if err != nil {
			t.Fatal(err)
		}
		if catalog != wantCatalog {
			t.Fatalf("catalog=%q", catalog)
		}
		captured = true
		return nil
	}}
	root.AddCommand(probe)
	root.SetArgs([]string{"--root", filepath.Join(rootDir, "custom-native"), "--runroot", filepath.Join(rootDir, "runtime"), "--storage-driver", "vfs", "--imagestore", filepath.Join(rootDir, "image-bytes"), "--transient-store", "storage-probe"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if !captured {
		t.Fatal("storage selection did not reach command")
	}
}

func TestImageStoreConfigAndFlagSelectEffectivePodmanStorage(t *testing.T) {
	rootDir := t.TempDir()
	configHome := filepath.Join(rootDir, "config")
	t.Setenv("XDG_CONFIG_HOME", configHome)
	storageConfig := filepath.Join(rootDir, "storage.conf")
	graph, run := filepath.Join(rootDir, "podman-graph"), filepath.Join(rootDir, "podman-run")
	data := []byte("[storage]\ndriver = \"vfs\"\ngraphroot = \"" + graph + "\"\nrunroot = \"" + run + "\"\n")
	if err := os.WriteFile(storageConfig, data, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONTAINERS_STORAGE_CONF", storageConfig)
	if err := os.MkdirAll(filepath.Join(configHome, "coopr"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configHome, "coopr", "config.toml"), []byte("image-store = \"podman\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	root := newRootCommand()
	var podmanCatalog string
	probe := &cobra.Command{Use: "storage-config-probe", RunE: func(cmd *cobra.Command, _ []string) error {
		store, catalog, err := commandStorage(cmd)
		if err != nil {
			return err
		}
		if !store.Shared || store.GraphRoot != graph || store.RunRoot != run || store.GraphDriverName != "vfs" {
			t.Fatalf("podman store = %+v", store)
		}
		if catalog == filepath.Join(graph, "coopr") {
			t.Fatalf("catalog was placed inside Podman graph: %s", catalog)
		}
		podmanCatalog = catalog
		return nil
	}}
	root.AddCommand(probe)
	root.SetArgs([]string{"storage-config-probe"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}

	root = newRootCommand()
	probe = &cobra.Command{Use: "storage-override-probe", RunE: func(cmd *cobra.Command, _ []string) error {
		store, catalog, err := commandStorage(cmd)
		if err != nil {
			return err
		}
		if store.Shared {
			t.Fatal("explicit coopr selection remained shared")
		}
		if catalog == podmanCatalog {
			t.Fatalf("coopr and Podman stores share catalog %s", catalog)
		}
		return nil
	}}
	root.AddCommand(probe)
	root.SetArgs([]string{"--image-store=coopr", "storage-override-probe"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
}

func TestBuildConsumesLocalImageWithGlobalStorageOverrides(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH=1 for native CLI storage builds")
	}
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	base := filepath.Join(dir, "base.coopr")
	child := filepath.Join(dir, "child.coopr")
	if err := os.WriteFile(base, []byte("from \"scratch\"\ncopy \"payload\" \"/payload\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(child, []byte("from \"local-base\"\nlabel inherited=\"yes\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "payload"), []byte("shared store"), 0600); err != nil {
		t.Fatal(err)
	}
	flags := []string{"--root", filepath.Join(dir, "native-arbitrary-name"), "--runroot", filepath.Join(dir, "run"), "--storage-driver", "vfs"}
	for _, args := range [][]string{{"build", base, "--tag", "local-base"}, {"build", child, "--tag", "local-child", "--pull=never"}, {"image", "inspect", "local-child"}} {
		var stdout, stderr bytes.Buffer
		if code := run(append(append([]string(nil), flags...), args...), &stdout, &stderr); code != 0 {
			t.Fatalf("%v: code=%d stdout=%s stderr=%s", args, code, &stdout, &stderr)
		}
		if args[0] == "image" && !strings.Contains(stdout.String(), "local-child:latest") {
			t.Fatalf("inspect=%s", &stdout)
		}
	}
}
