package buildah

import (
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
