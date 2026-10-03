package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadCooprConfigDefaultsAndParsesImageStore(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", root)
	config, err := loadCooprConfig()
	if err != nil || config.ImageStore != imageStoreCoopr {
		t.Fatalf("missing config = %+v, %v", config, err)
	}
	if err := os.MkdirAll(filepath.Join(root, "coopr"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "coopr", "config.toml"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	config, err = loadCooprConfig()
	if err != nil || config.ImageStore != imageStoreCoopr {
		t.Fatalf("empty config = %+v, %v", config, err)
	}
	if err := os.WriteFile(filepath.Join(root, "coopr", "config.toml"), []byte("image-store = \"Podman\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err = loadCooprConfig()
	if err != nil || config.ImageStore != imageStorePodman {
		t.Fatalf("podman config = %+v, %v", config, err)
	}
}

func TestLoadCooprConfigRejectsUnknownAndInvalidValues(t *testing.T) {
	for _, data := range []string{"unknown = true\n", "image-store = \"\"\n", "image-store = \"docker\"\n", "image-store = [\"podman\"]\n", "image-store = \"coopr\"\nimage-store = \"podman\"\n"} {
		t.Run(strings.ReplaceAll(data, " ", "_"), func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("XDG_CONFIG_HOME", root)
			if err := os.MkdirAll(filepath.Join(root, "coopr"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "coopr", "config.toml"), []byte(data), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := loadCooprConfig(); err == nil {
				t.Fatalf("configuration %q was accepted", data)
			}
		})
	}
}
