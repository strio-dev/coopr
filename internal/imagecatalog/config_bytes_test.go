package imagecatalog

import (
	"bytes"
	"context"
	"testing"

	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestCatalogPreservesExactConfigBytes(t *testing.T) {
	for _, config := range []string{
		"{\n  \"os\": \"linux\", \"architecture\": \"amd64\"\n}\n",
		`{"os":"linux","architecture":"amd64","config":{"Env":["MESSAGE=<hello>&goodbye"]}}`,
	} {
		t.Run(config, func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			platform := v1.Platform{OS: "linux", Architecture: "amd64"}
			root := descriptor(v1.MediaTypeImageManifest, "config-bytes")
			selection := testSelection(root, root, config)
			if err := Commit(ctx, dir, "registry.example/base:latest", platform, selection); err != nil {
				t.Fatal(err)
			}
			loaded, found, err := Lookup(ctx, dir, "registry.example/base:latest", platform)
			if err != nil || !found {
				t.Fatalf("reload exact config: found=%t error=%v", found, err)
			}
			if !bytes.Equal(loaded.ConfigData, []byte(config)) || loaded.ImageID != selection.ImageID {
				t.Fatalf("config bytes or image identity changed: config=%q id=%s", loaded.ConfigData, loaded.ImageID)
			}
		})
	}
}
