package build

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	digest "github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale"
	"github.com/strio-dev/coopr/internal/oci"
)

func TestBuildComponentCompleteBundleColdWarmAndFailure(t *testing.T) {
	if testing.Short() {
		t.Skip("requires actual native package builds")
	}
	root := t.TempDir()
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
	context := filepath.Join(root, "context")
	if err := os.Mkdir(context, 0700); err != nil {
		t.Fatal(err)
	}
	definition := filepath.Join(context, "component.coopr")
	if err := os.WriteFile(definition, []byte(`package as="files" {
    copy "payload" "/payload"
}
extend {
    copy "/payload" "/installed" from="files"
    env BUNDLE_CHECK="preserved"
}
`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(context, "payload"), []byte("complete input\n"), 0600); err != nil {
		t.Fatal(err)
	}
	stamp := int64(123456789)
	options := ComponentOptions{File: definition, Platform: "linux/amd64", Timestamp: &stamp, StoreDir: filepath.Join(root, "components"), BuildStore: nativeBuildTestStore(filepath.Join(root, "images")), CacheLocalDir: filepath.Join(root, "cache")}
	client, err := bale.NewClient(bale.ClientOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var previousRoot, previousBundle digest.Digest
	type metadata struct {
		Digest digest.Digest `json:"containerimage.digest"`
		Bundle struct {
			Path       string        `json:"path"`
			Descriptor v1.Descriptor `json:"descriptor"`
			Error      string        `json:"error"`
		} `json:"coopr.bundle"`
		Error    string `json:"coopr.outputError"`
		Progress []struct {
			Cache string `json:"cache"`
		} `json:"coopr.operation.progress"`
	}
	readMetadata := func(path string) metadata {
		t.Helper()
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var result metadata
		if err := json.Unmarshal(data, &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	for _, phase := range []string{"cold", "warm"} {
		options.Tags = []string{"local-" + phase}
		options.MetadataFile = filepath.Join(root, phase+".json")
		options.Bundle = &oci.ComponentBundleOptions{Output: filepath.Join(root, phase+".bale")}
		if _, err := BuildComponent(t.Context(), options); err != nil {
			t.Fatal(err)
		}
		result := readMetadata(options.MetadataFile)
		hit := false
		for _, progress := range result.Progress {
			hit = hit || progress.Cache == "hit"
		}
		if phase == "warm" && !hit {
			t.Fatal("warm bundle build did not report a Coopr cache hit")
		}
		t.Logf("phase=%s nativeCacheHit=%t", phase, hit)
		verified, err := client.InspectLayout(t.Context(), options.Bundle.Output)
		if err != nil || verified.MetadataOnly || result.Bundle.Error != "" || result.Error != "" || result.Bundle.Descriptor.Digest != verified.Descriptor.Digest || result.Bundle.Path != options.Bundle.Output || len(verified.Metadata.Roots) != 1 || verified.Metadata.Roots[0].Descriptor.Digest != result.Digest {
			t.Fatalf("complete build output metadata: %+v, %+v, %v", result, verified, err)
		}
		if previousRoot != "" && (result.Digest != previousRoot || result.Bundle.Descriptor.Digest != previousBundle) {
			t.Fatalf("warm build/local alias changed immutable result: %+v", result)
		}
		previousRoot, previousBundle = result.Digest, result.Bundle.Descriptor.Digest
	}
	if _, err := BuildComponent(t.Context(), options); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("existing bundle was overwritten: %v", err)
	}
	options.Bundle = &oci.ComponentBundleOptions{Output: filepath.Join(root, "failed.bale"), Components: map[string]string{"coopr.invalid/missing@" + digest.FromString("missing").String(): "not-loaded"}}
	options.MetadataFile = filepath.Join(root, "failed.json")
	if _, err := BuildComponent(t.Context(), options); err == nil || !strings.Contains(err.Error(), "component retained") {
		t.Fatalf("bundle failure did not report retained component: %v", err)
	}
	failed := readMetadata(options.MetadataFile)
	if failed.Digest != previousRoot || failed.Error == "" || failed.Bundle.Error == "" || failed.Bundle.Descriptor.Digest != "" {
		t.Fatalf("bundle failure metadata claims success or loses root: %+v", failed)
	}
	if _, err := os.Lstat(options.Bundle.Output); !os.IsNotExist(err) {
		t.Fatalf("failed bundle exposed output: %v", err)
	}
}
