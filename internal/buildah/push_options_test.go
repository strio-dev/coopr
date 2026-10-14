package buildah

import (
	"encoding/json"
	"go.podman.io/common/pkg/config"
	"os"
	"path/filepath"
	"testing"

	imagecopy "go.podman.io/image/v5/copy"
	"go.podman.io/image/v5/types"
)

func TestPushOptionsNativeMapping(t *testing.T) {
	_, public := testEncryptionKeys(t, t.TempDir())
	level := 3
	options := PushOptions{Format: "oci", CompressionFormat: "zstd", CompressionLevel: &level, ForceCompression: true, EncryptionKeys: []string{"jwe:" + public}, EncryptLayers: []int{-1}, RemoveSignatures: true}
	native := &imagecopy.Options{PreserveDigests: true, DestinationCtx: &types.SystemContext{}}
	if err := options.Apply(native); err != nil {
		t.Fatal(err)
	}
	if native.PreserveDigests || native.DestinationCtx.CompressionFormat.Name() != "zstd" || *native.DestinationCtx.CompressionLevel != 3 || !native.ForceCompressionFormat || native.OciEncryptConfig == nil || native.OciEncryptLayers == nil || (*native.OciEncryptLayers)[0] != -1 || !native.RemoveSignatures {
		t.Fatalf("native copy options=%+v", native)
	}
	variants := &imagecopy.Options{}
	if err := (PushOptions{AddCompression: []string{"gzip", "zstd"}}).Apply(variants); err != nil {
		t.Fatal(err)
	}
	if len(variants.EnsureCompressionVariantsExist) != 2 {
		t.Fatalf("variants=%v", variants.EnsureCompressionVariantsExist)
	}
	defaults := &imagecopy.Options{PreserveDigests: true}
	if err := (PushOptions{ForceCompression: true}).Apply(defaults); err != nil {
		t.Fatal(err)
	}
	if defaults.DestinationCtx.CompressionFormat == nil || !defaults.ForceCompressionFormat || defaults.PreserveDigests {
		t.Fatalf("default forced compression=%+v", defaults)
	}
}

func TestPushUsesConfiguredCompressionDefaults(t *testing.T) {
	module := filepath.Join(t.TempDir(), "push.conf")
	if err := os.WriteFile(module, []byte("[engine]\ncompression_format='zstd'\ncompression_level=3\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = config.New(&config.Options{SetDefault: true}) })
	if _, err := config.New(&config.Options{Modules: []string{module}, SetDefault: true}); err != nil {
		t.Fatal(err)
	}
	native := &imagecopy.Options{PreserveDigests: true}
	if err := (PushOptions{}).Apply(native); err != nil {
		t.Fatal(err)
	}
	if native.DestinationCtx.CompressionFormat == nil || native.DestinationCtx.CompressionFormat.Name() != "zstd" || native.DestinationCtx.CompressionLevel == nil || *native.DestinationCtx.CompressionLevel != 3 || !native.ForceCompressionFormat || native.PreserveDigests {
		t.Fatalf("copy=%+v system=%+v", native, native.DestinationCtx)
	}
}

func TestPushExplicitCompressionOverridesConfiguration(t *testing.T) {
	module := filepath.Join(t.TempDir(), "push.conf")
	if err := os.WriteFile(module, []byte("[engine]\ncompression_format='zstd'\ncompression_level=3\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = config.New(&config.Options{SetDefault: true}) })
	if _, err := config.New(&config.Options{Modules: []string{module}, SetDefault: true}); err != nil {
		t.Fatal(err)
	}
	level := 1
	native := &imagecopy.Options{}
	if err := (PushOptions{CompressionFormat: "gzip", CompressionLevel: &level, ForceCompressionSet: true}).Apply(native); err != nil {
		t.Fatal(err)
	}
	if native.DestinationCtx.CompressionFormat.Name() != "gzip" || *native.DestinationCtx.CompressionLevel != 1 || native.ForceCompressionFormat {
		t.Fatalf("explicit compression overridden: %+v", native)
	}
}

func TestPushResolvedDefaultsSurviveWorkerSerialization(t *testing.T) {
	module := filepath.Join(t.TempDir(), "push.conf")
	if err := os.WriteFile(module, []byte("[engine]\ncompression_format='gzip'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = config.New(&config.Options{SetDefault: true}) })
	if _, err := config.New(&config.Options{Modules: []string{module}, SetDefault: true}); err != nil {
		t.Fatal(err)
	}
	resolved, err := (PushOptions{}).WithDefaults()
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(resolved)
	if err != nil {
		t.Fatal(err)
	}
	var worker PushOptions
	if err := json.Unmarshal(data, &worker); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(module, []byte("[engine]\ncompression_format='zstd'\ncompression_level=3\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := config.New(&config.Options{Modules: []string{module}, SetDefault: true}); err != nil {
		t.Fatal(err)
	}
	native := &imagecopy.Options{PreserveDigests: true}
	if err := worker.Apply(native); err != nil {
		t.Fatal(err)
	}
	if !native.PreserveDigests || native.DestinationCtx.CompressionFormat != nil || native.DestinationCtx.CompressionLevel != nil {
		t.Fatalf("worker reloaded defaults: %+v", native)
	}
}

func TestPushCompressionLevelDoesNotForceDefaultFormat(t *testing.T) {
	module := filepath.Join(t.TempDir(), "gzip.conf")
	if err := os.WriteFile(module, []byte("[engine]\ncompression_format='gzip'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = config.New(&config.Options{SetDefault: true}) })
	if _, err := config.New(&config.Options{Modules: []string{module}, SetDefault: true}); err != nil {
		t.Fatal(err)
	}
	level := 3
	native := &imagecopy.Options{}
	if err := (PushOptions{CompressionLevel: &level}).Apply(native); err != nil {
		t.Fatal(err)
	}
	if native.DestinationCtx.CompressionFormat != nil || native.ForceCompressionFormat || native.DestinationCtx.CompressionLevel == nil || *native.DestinationCtx.CompressionLevel != 3 {
		t.Fatalf("level forced format: %+v", native)
	}
}

func TestPushCompressionTransportMapping(t *testing.T) {
	for _, test := range []struct {
		options            PushOptions
		compress, preserve bool
	}{
		{PushOptions{CompressionDefaultsResolved: true}, false, true},
		{PushOptions{CompressionDefaultsResolved: true, Compress: true}, true, false},
	} {
		data, err := json.Marshal(test.options)
		if err != nil {
			t.Fatal(err)
		}
		var worker PushOptions
		if err := json.Unmarshal(data, &worker); err != nil {
			t.Fatal(err)
		}
		native := &imagecopy.Options{PreserveDigests: true}
		if err := worker.Apply(native); err != nil {
			t.Fatal(err)
		}
		if native.DestinationCtx.DirForceCompress != test.compress || native.PreserveDigests != test.preserve {
			t.Fatalf("%+v: copy=%+v context=%+v", worker, native, native.DestinationCtx)
		}
	}
}
