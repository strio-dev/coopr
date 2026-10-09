package build

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"coopr/internal/buildah"
	"coopr/internal/oci"
	"coopr/internal/transfer"
	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestOutputDestinationsValidateAndDeduplicate(t *testing.T) {
	destinations, err := outputDestinations(oci.Image, "app:latest", []string{"app:latest", "app:stable", "docker:app:test"}, false)
	if err != nil || len(destinations) != 3 {
		t.Fatalf("destinations = %v, %v", destinations, err)
	}
	if _, err := outputDestinations(oci.Component, "", []string{"docker:bad"}, false); err == nil {
		t.Fatal("engine component output accepted")
	}
	if _, err := outputDestinations(oci.Image, "", []string{"docker:bad"}, true); err == nil {
		t.Fatal("push with engine destination accepted")
	}
	destinations, err = outputDestinations(oci.Image, "", []string{"podman:dev"}, false)
	if err != nil || len(destinations) != 1 || destinations[0] != (transfer.Destination{Transport: "local", Name: "podman:dev"}) {
		t.Fatalf("ordinary image output = %+v, %v", destinations, err)
	}
}

func TestDestinationFailureReportsCommittedAndPendingResults(t *testing.T) {
	for _, cancel := range []bool{false, true} {
		t.Run(fmt.Sprint(cancel), func(t *testing.T) {
			ctx, stop := context.WithCancel(context.Background())
			defer stop()
			root := v1.Descriptor{Digest: digest.FromString("built")}
			destinations := []transfer.Destination{{Transport: "local", Name: "first"}, {Transport: "registry", Name: "second"}, {Transport: "local", Name: "third"}}
			calls := 0
			report, publicationErr := publishDestinations(ctx, oci.Image, root, destinations, func(_ context.Context, destination transfer.Destination) (string, error) {
				calls++
				if calls == 1 {
					if cancel {
						stop()
					}
					return destination.Name, nil
				}
				return "", errors.New("registry refused output")
			})
			if publicationErr == nil || len(report.References) != 1 || report.References[0] != "first" {
				t.Fatalf("partial publication = %+v, %v", report, publicationErr)
			}
			if report.Destinations[0].Status != "complete" || report.Destinations[1].Status != "failed" || report.Destinations[2].Status != "pending" {
				t.Fatalf("destination states = %+v", report.Destinations)
			}
			metadata := filepath.Join(t.TempDir(), "result.json")
			_, err := finishOutputs(metadata, "", root, nil, nil, report, publicationErr, "")
			if err == nil {
				t.Fatal("partial publication returned success")
			}
			data, err := os.ReadFile(metadata)
			if err != nil || !strings.Contains(string(data), `"coopr.outputError"`) || !strings.Contains(string(data), root.Digest.String()) || !strings.Contains(string(data), `"status": "pending"`) {
				t.Fatalf("failure metadata = %s, %v", data, err)
			}
		})
	}
}

func TestResultOutputPreflightAndFinalizationFailures(t *testing.T) {
	root := t.TempDir()
	store := filepath.Join(root, "store")
	if err := os.Mkdir(store, 0700); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{store, filepath.Join(store, "storage.lock"), filepath.Join(root, "missing", "result.json")} {
		if err := preflightOutputArtifacts([]string{path}, store); err == nil {
			t.Fatalf("invalid result path accepted: %s", path)
		}
	}
	metadata, iid := filepath.Join(root, "result.json"), filepath.Join(root, "iid")
	if err := preflightOutputArtifacts([]string{metadata, iid}, store); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(metadata); !os.IsNotExist(err) {
		t.Fatalf("preflight replaced output: %v", err)
	}
	// A path can become unwritable after preflight; the failure must identify
	// the durable result and completed destinations rather than hide them.
	if err := os.Mkdir(metadata, 0700); err != nil {
		t.Fatal(err)
	}
	descriptor := v1.Descriptor{Digest: digest.FromString("retained")}
	report := outputReport{References: []string{"app:latest"}}
	if _, err := finishOutputs(metadata, iid, descriptor, nil, nil, report, nil, ""); err == nil || !strings.Contains(err.Error(), descriptor.Digest.String()) || !strings.Contains(err.Error(), "app:latest") {
		t.Fatalf("metadata finalization failure = %v", err)
	}
	if data, err := os.ReadFile(iid); err != nil || strings.TrimSpace(string(data)) != descriptor.Digest.String() {
		t.Fatalf("independent IID was not written after metadata failure: %s, %v", data, err)
	}
	if err := os.Remove(metadata); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(iid); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(iid, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := finishOutputs(metadata, iid, descriptor, nil, nil, report, nil, ""); err == nil || !strings.Contains(err.Error(), descriptor.Digest.String()) {
		t.Fatalf("IID finalization failure = %v", err)
	}
}

func TestOutputPreflightUsesConfiguredNativeRoots(t *testing.T) {
	for _, separateImages := range []bool{false, true} {
		t.Run(fmt.Sprint(separateImages), func(t *testing.T) {
			workspace := t.TempDir()
			t.Chdir(workspace)
			store := buildah.StoreOptions{GraphRoot: t.TempDir(), RunRoot: t.TempDir()}
			if separateImages {
				store.ImageStore = t.TempDir()
			}
			roots := buildah.ActivityRoots(store, t.TempDir())
			archive := filepath.Join(workspace, "image.oci.tar")
			if err := preflightOutputArtifacts([]string{archive}, roots...); err != nil {
				t.Fatalf("archive outside native storage rejected: %v", err)
			}
			if _, err := os.Stat(archive); !os.IsNotExist(err) {
				t.Fatalf("preflight modified archive output: %v", err)
			}
			definition := filepath.Join(workspace, "image.coopr")
			filesystem := buildah.FilesystemOutput{Type: "local", Path: filepath.Join(workspace, "rootfs")}
			if err := preflightFilesystemOutput(filesystem, definition, roots...); err != nil {
				t.Fatalf("filesystem outside native storage rejected: %v", err)
			}
			for _, root := range roots {
				if err := preflightOutputArtifacts([]string{filepath.Join(root, "image.oci.tar")}, roots...); err == nil {
					t.Fatalf("output inside native/component root %q accepted", root)
				}
				filesystem.Path = filepath.Join(root, "rootfs")
				if err := preflightFilesystemOutput(filesystem, definition, roots...); err == nil {
					t.Fatalf("filesystem inside native/component root %q accepted", root)
				}
			}
		})
	}
}

func TestMetadataIncludesIndexAndPlatformConfigurationDigests(t *testing.T) {
	root := v1.Descriptor{MediaType: v1.MediaTypeImageIndex, Digest: digest.FromString("index"), Size: 24}
	manifest := v1.Descriptor{MediaType: v1.MediaTypeImageManifest, Digest: digest.FromString("instance"), Size: 12}
	platform := v1.Platform{OS: "linux", Architecture: "amd64"}
	configData := []byte(`{"architecture":"amd64","os":"linux","config":{}}`)
	config := digest.FromBytes(configData)
	nativeID := digest.FromString("native image record").Encoded()
	variants := []oci.IndexVariant{{Manifest: manifest, Platform: platform}}
	selections := map[string]oci.StoredSelection{"linux/amd64": {Manifest: manifest, ImageID: nativeID, ConfigData: configData}}
	directory := t.TempDir()
	metadata, iid := filepath.Join(directory, "result.json"), filepath.Join(directory, "iid")
	if err := writeOutputMetadata(metadata, iid, root, variants, selections, outputReport{References: []string{"app:latest"}}, nil, ""); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(metadata)
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]json.RawMessage
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	if string(result["containerimage.config.digest"]) != `"`+config.String()+`"` {
		t.Fatalf("config digest = %s, want %s", result["containerimage.config.digest"], config)
	}
	if string(result["containerimage.digest"]) != `"`+root.Digest.String()+`"` || !strings.Contains(string(result["coopr.platforms"]), config.String()) {
		t.Fatalf("metadata = %s", data)
	}
	data, err = os.ReadFile(iid)
	if err != nil || strings.TrimSpace(string(data)) != "sha256:"+nativeID {
		t.Fatalf("iid = %s, %v", data, err)
	}
}

func TestMetadataMultiPlatformIIDUsesIndexDigest(t *testing.T) {
	root := v1.Descriptor{MediaType: v1.MediaTypeImageIndex, Digest: digest.FromString("index")}
	variants := []oci.IndexVariant{
		{Manifest: v1.Descriptor{Digest: digest.FromString("amd64 manifest")}, Platform: v1.Platform{OS: "linux", Architecture: "amd64"}},
		{Manifest: v1.Descriptor{Digest: digest.FromString("arm64 manifest")}, Platform: v1.Platform{OS: "linux", Architecture: "arm64"}},
	}
	configs := map[string][]byte{
		"linux/amd64": []byte(`{"architecture":"amd64","os":"linux"}`),
		"linux/arm64": []byte(`{"architecture":"arm64","os":"linux"}`),
	}
	selections := map[string]oci.StoredSelection{}
	for platform, data := range configs {
		selections[platform] = oci.StoredSelection{ImageID: digest.FromString(platform + " native record").Encoded(), ConfigData: data}
	}
	directory := t.TempDir()
	metadata, iid := filepath.Join(directory, "metadata.json"), filepath.Join(directory, "iid")
	if err := writeOutputMetadata(metadata, iid, root, variants, selections, outputReport{}, nil, ""); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(metadata)
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		ConfigDigest string `json:"containerimage.config.digest"`
		Platforms    []struct {
			Platform     string `json:"platform"`
			ConfigDigest string `json:"configDigest"`
		} `json:"coopr.platforms"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	if result.ConfigDigest != "" || len(result.Platforms) != len(configs) {
		t.Fatalf("multi-platform metadata = %s", data)
	}
	for _, entry := range result.Platforms {
		if entry.ConfigDigest != digest.FromBytes(configs[entry.Platform]).String() {
			t.Fatalf("platform %s config digest = %s", entry.Platform, entry.ConfigDigest)
		}
	}
	data, err = os.ReadFile(iid)
	if err != nil || strings.TrimSpace(string(data)) != root.Digest.String() {
		t.Fatalf("index iid = %s, %v", data, err)
	}
}

func TestOutputMetadataWritesRawNativeID(t *testing.T) {
	root := v1.Descriptor{Digest: digest.FromString("manifest")}
	rawPath := filepath.Join(t.TempDir(), "raw-iid")
	platform := v1.Platform{OS: "linux", Architecture: "amd64"}
	variants := []oci.IndexVariant{{Platform: platform, Manifest: root}}
	selections := map[string]oci.StoredSelection{"linux/amd64": {ImageID: strings.Repeat("a", 64)}}
	if err := writeOutputMetadata("", "", root, variants, selections, outputReport{}, nil, rawPath); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(rawPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != strings.Repeat("a", 64) {
		t.Fatalf("raw ID: %q", data)
	}
}

func TestRawIIDFailurePreservesMetadata(t *testing.T) {
	root := v1.Descriptor{Digest: digest.FromString("retained")}
	directory := t.TempDir()
	metadata := filepath.Join(directory, "metadata.json")
	rawIID := filepath.Join(directory, "raw-iid")
	if err := os.Mkdir(rawIID, 0700); err != nil {
		t.Fatal(err)
	}
	err := writeOutputMetadata(metadata, "", root, nil, nil, outputReport{}, nil, rawIID)
	if err == nil || !strings.Contains(err.Error(), "write raw image ID") {
		t.Fatalf("raw IID failure = %v", err)
	}
	data, err := os.ReadFile(metadata)
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	if diagnostic, ok := result["coopr.outputError"].(string); !ok || !strings.Contains(diagnostic, "write raw image ID") {
		t.Fatalf("metadata diagnostic = %s", data)
	}
}
