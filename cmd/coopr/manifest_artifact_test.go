package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/strio-dev/coopr/internal/buildah"
	"github.com/strio-dev/coopr/internal/oci"
	"github.com/strio-dev/coopr/internal/transfer"
	"go.podman.io/storage"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	imagestorage "go.podman.io/image/v5/storage"
)

func TestManifestArtifactNativeRoundtrip(t *testing.T) {
	if testing.Short() {
		t.Skip("native disposable store and registry")
	}
	previousStore := imagestorage.Transport.GetStoreIfSet()
	t.Cleanup(func() {
		if imagestorage.Transport.GetStoreIfSet() != previousStore {
			t.Error("manifest push changed the ambient native storage binding")
		}
	})
	options := maintenanceStoreOptions(t.TempDir())
	payload := filepath.Join(t.TempDir(), "payload.txt")
	if err := os.WriteFile(payload, []byte("artifact payload\n"), 0600); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(config, []byte(`{"test":"config"}`), 0600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(registry.New())
	defer server.Close()
	var out, errs bytes.Buffer
	execute := func(args ...string) {
		t.Helper()
		out.Reset()
		errs.Reset()
		if status := run(manifestTestArgs(options, args...), &out, &errs); status != 0 {
			t.Fatalf("%v: status %d: %s", args, status, &errs)
		}
	}
	execute("create", "artifact-list")
	execute("add", "--artifact", "--artifact-type", "application/vnd.coopr.test", "--artifact-config", config, "--artifact-config-type", "application/json", "--artifact-layer-type", "text/plain", "--artifact-annotation", "manifest=value", "--annotation", "entry=value", "artifact-list", payload)
	execute("inspect", "artifact-list")
	var index v1.Index
	if err := json.Unmarshal(out.Bytes(), &index); err != nil {
		t.Fatal(err)
	}
	if len(index.Manifests) != 1 || index.Manifests[0].ArtifactType != "application/vnd.coopr.test" || index.Manifests[0].Annotations["entry"] != "value" {
		t.Fatalf("artifact index: %s", &out)
	}
	destination := strings.TrimPrefix(server.URL, "http://") + "/artifacts:test"
	execute("push", "--tls-verify=false", "--format", "oci", "artifact-list", destination)
	execute("inspect", "--tls-verify=false", destination)
	var remote v1.Index
	if err := json.Unmarshal(out.Bytes(), &remote); err != nil {
		t.Fatal(err)
	}
	if len(remote.Manifests) != 1 || remote.Manifests[0].Digest != index.Manifests[0].Digest {
		t.Fatalf("remote artifact changed: %s", &out)
	}
	archive := filepath.Join(t.TempDir(), "artifacts.tar")
	execute("push", "--format", "oci", "artifact-list", "oci-archive:"+archive+":test")
	if info, err := os.Stat(archive); err != nil || info.Size() == 0 {
		t.Fatalf("artifact archive: %v, %v", info, err)
	}
	saved := filepath.Join(t.TempDir(), "saved")
	execute("push", "--format", "oci", "artifact-list", "oci:"+saved+":test")
	data, err := os.ReadFile(filepath.Join(saved, "blobs", "sha256", index.Manifests[0].Digest.Encoded()))
	if err != nil {
		t.Fatal(err)
	}
	var artifact v1.Manifest
	if err := json.Unmarshal(data, &artifact); err != nil {
		t.Fatal(err)
	}
	if artifact.Annotations["manifest"] != "value" || artifact.Config.MediaType != "application/json" || len(artifact.Layers) != 1 || artifact.Layers[0].MediaType != "text/plain" || artifact.Layers[0].Annotations[v1.AnnotationTitle] != "payload.txt" {
		t.Fatalf("artifact = %s", data)
	}
	blob, err := os.ReadFile(filepath.Join(saved, "blobs", "sha256", artifact.Layers[0].Digest.Encoded()))
	if err != nil || string(blob) != "artifact payload\n" {
		t.Fatalf("saved payload %q: %v", blob, err)
	}
}

func TestManifestArtifactEmptyConfig(t *testing.T) {
	if testing.Short() {
		t.Skip("native disposable store")
	}
	previousStore := imagestorage.Transport.GetStoreIfSet()
	t.Cleanup(func() {
		if imagestorage.Transport.GetStoreIfSet() != previousStore {
			t.Error("manifest push changed the ambient native storage binding")
		}
	})
	options := maintenanceStoreOptions(t.TempDir())
	config := filepath.Join(t.TempDir(), "empty")
	if err := os.WriteFile(config, nil, 0600); err != nil {
		t.Fatal(err)
	}
	var out, errs bytes.Buffer
	if code := run(manifestTestArgs(options, "create", "empty-config"), &out, &errs); code != 0 {
		t.Fatalf("create: %s", &errs)
	}
	out.Reset()
	errs.Reset()
	if code := run(manifestTestArgs(options, "add", "--artifact", "--artifact-config", config, "empty-config", config), &out, &errs); code != 0 {
		t.Fatalf("add zero-byte config: %s", &errs)
	}
	out.Reset()
	errs.Reset()
	if code := run(manifestTestArgs(options, "inspect", "empty-config"), &out, &errs); code != 0 {
		t.Fatalf("inspect: %s", &errs)
	}
	var index v1.Index
	if err := json.Unmarshal(out.Bytes(), &index); err != nil {
		t.Fatal(err)
	}
	saved := filepath.Join(t.TempDir(), "saved")
	out.Reset()
	errs.Reset()
	if code := run(manifestTestArgs(options, "push", "--format", "oci", "empty-config", "oci:"+saved+":test"), &out, &errs); code != 0 {
		t.Fatalf("export: %s", &errs)
	}
	raw, err := os.ReadFile(filepath.Join(saved, "blobs", "sha256", index.Manifests[0].Digest.Encoded()))
	if err != nil {
		t.Fatal(err)
	}
	var artifact v1.Manifest
	if err := json.Unmarshal(raw, &artifact); err != nil {
		t.Fatal(err)
	}
	if artifact.Config.Size != 0 {
		t.Fatalf("empty config became %d bytes", artifact.Config.Size)
	}
	data, err := os.ReadFile(filepath.Join(saved, "blobs", "sha256", artifact.Config.Digest.Encoded()))
	if err != nil || len(data) != 0 {
		t.Fatalf("empty config bytes %q: %v", data, err)
	}
}

func TestManifestArtifactWorkersPreserveUnrelatedParentStore(t *testing.T) {
	if testing.Short() {
		t.Skip("native isolated storage workers")
	}
	unrelated := maintenanceStoreOptions(t.TempDir())
	if err := buildah.WithStore(unrelated, func(parent storage.Store) error {
		previous := imagestorage.Transport.GetStoreIfSet()
		imagestorage.Transport.SetStore(parent)
		defer imagestorage.Transport.SetStore(previous)
		type fixture struct {
			options         buildah.StoreOptions
			output, payload string
		}
		fixtures := make([]fixture, 2)
		for i := range fixtures {
			options := maintenanceStoreOptions(t.TempDir())
			payload := fmt.Sprintf("artifact from store %d", i)
			file := filepath.Join(t.TempDir(), "payload.txt")
			if err := os.WriteFile(file, []byte(payload), 0600); err != nil {
				return err
			}
			for _, args := range [][]string{{"create", "artifact"}, {"add", "--artifact", "artifact", file}} {
				var out, errs bytes.Buffer
				if status := run(manifestTestArgs(options, args...), &out, &errs); status != 0 {
					return fmt.Errorf("prepare %v: %s", args, &errs)
				}
			}
			fixtures[i] = fixture{options: options, output: filepath.Join(t.TempDir(), "saved"), payload: payload}
		}
		completed := make(chan error, len(fixtures))
		for _, fixture := range fixtures {
			go func() {
				_, err := transfer.PushManifestList(t.Context(), "artifact", transfer.Destination{Transport: "oci", Name: fixture.output, ArchiveReference: "test"}, transfer.Options{BuildStore: fixture.options}, true, nil)
				completed <- err
			}()
		}
		var pushErr error
		for range fixtures {
			pushErr = errors.Join(pushErr, <-completed)
		}
		if pushErr != nil {
			return pushErr
		}
		if imagestorage.Transport.GetStoreIfSet() != parent {
			return fmt.Errorf("worker push changed parent storage binding")
		}
		for _, fixture := range fixtures {
			root, err := oci.LayoutRoot(fixture.output)
			if err != nil {
				return err
			}
			indexData, err := os.ReadFile(filepath.Join(fixture.output, "blobs", "sha256", root.Digest.Encoded()))
			if err != nil {
				return err
			}
			var index v1.Index
			if err := json.Unmarshal(indexData, &index); err != nil {
				return err
			}
			manifestData, err := os.ReadFile(filepath.Join(fixture.output, "blobs", "sha256", index.Manifests[0].Digest.Encoded()))
			if err != nil {
				return err
			}
			var artifact v1.Manifest
			if err := json.Unmarshal(manifestData, &artifact); err != nil {
				return err
			}
			data, err := os.ReadFile(filepath.Join(fixture.output, "blobs", "sha256", artifact.Layers[0].Digest.Encoded()))
			if err != nil {
				return err
			}
			if string(data) != fixture.payload {
				return fmt.Errorf("artifact store selection changed: %q, want %q", data, fixture.payload)
			}
		}
		_, err := transfer.PushManifestList(t.Context(), "missing-list", transfer.Destination{Transport: "oci", Name: filepath.Join(t.TempDir(), "failed")}, transfer.Options{BuildStore: fixtures[0].options}, true, nil)
		if err == nil {
			return fmt.Errorf("missing manifest list push succeeded")
		}
		if imagestorage.Transport.GetStoreIfSet() != parent {
			return fmt.Errorf("failed worker push changed parent storage binding")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
