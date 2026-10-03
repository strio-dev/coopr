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

	"coopr/internal/imagecatalog"
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
	if _, err := outputDestinations(oci.Image, "", []string{"podman:bad"}, true); err == nil {
		t.Fatal("push with engine destination accepted")
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
			_, err := finishOutputs(metadata, "", root, nil, nil, report, publicationErr)
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
	for _, path := range []string{store, filepath.Join(store, "catalog.json"), filepath.Join(root, "missing", "result.json")} {
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
	if _, err := finishOutputs(metadata, iid, descriptor, nil, nil, report, nil); err == nil || !strings.Contains(err.Error(), descriptor.Digest.String()) || !strings.Contains(err.Error(), "app:latest") {
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
	if _, err := finishOutputs(metadata, iid, descriptor, nil, nil, report, nil); err == nil || !strings.Contains(err.Error(), descriptor.Digest.String()) {
		t.Fatalf("IID finalization failure = %v", err)
	}
}

func TestMetadataIncludesIndexAndPlatformConfigurationDigests(t *testing.T) {
	root := v1.Descriptor{MediaType: v1.MediaTypeImageIndex, Digest: digest.FromString("index"), Size: 24}
	manifest := v1.Descriptor{MediaType: v1.MediaTypeImageManifest, Digest: digest.FromString("instance"), Size: 12}
	platform := v1.Platform{OS: "linux", Architecture: "amd64"}
	config := digest.FromString("config")
	variants := []oci.IndexVariant{{Manifest: manifest, Platform: platform}}
	selections := map[string]imagecatalog.Selection{"linux/amd64": {Manifest: manifest, ImageID: config.Encoded()}}
	directory := t.TempDir()
	metadata, iid := filepath.Join(directory, "result.json"), filepath.Join(directory, "iid")
	if err := writeOutputMetadata(metadata, iid, root, variants, selections, outputReport{References: []string{"app:latest"}}, nil); err != nil {
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
	if string(result["containerimage.digest"]) != `"`+root.Digest.String()+`"` || !strings.Contains(string(result["coopr.platforms"]), config.String()) {
		t.Fatalf("metadata = %s", data)
	}
	data, err = os.ReadFile(iid)
	if err != nil || strings.TrimSpace(string(data)) != config.String() {
		t.Fatalf("iid = %s, %v", data, err)
	}
}
