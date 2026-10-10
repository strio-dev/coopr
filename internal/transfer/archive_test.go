package transfer

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"coopr/internal/localstore"
	"coopr/internal/oci"
	"github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"go.podman.io/image/v5/manifest"
	"oras.land/oras-go/v2/content"
	orasoci "oras.land/oras-go/v2/content/oci"
)

func TestImageArchivePreservesDockerGraphsAndAllowsExplicitConversion(t *testing.T) {
	ctx := context.Background()
	for _, kind := range []string{"single", "docker-index", "oci-index", "nested-index"} {
		for _, transport := range []string{"oci-archive", "oci-dir"} {
			for _, convert := range []bool{false, true} {
				if kind == "nested-index" && convert {
					continue
				}
				t.Run(kind+"/"+transport+"/convert="+map[bool]string{false: "false", true: "true"}[convert], func(t *testing.T) {
					sourcePath := filepath.Join(t.TempDir(), "source")
					source, err := orasoci.NewWithContext(ctx, sourcePath)
					if err != nil {
						t.Fatal(err)
					}
					push := func(media string, data []byte) v1.Descriptor {
						desc := oci.Descriptor(media, data)
						if err := source.Push(ctx, desc, bytes.NewReader(data)); err != nil {
							t.Fatal(err)
						}
						return desc
					}
					configs := []v1.Descriptor{}
					children := []v1.Descriptor{}
					for _, arch := range []string{"amd64", "arm64"} {
						config := push(manifest.DockerV2Schema2ConfigMediaType, []byte(`{"architecture":"`+arch+`","os":"linux","rootfs":{"type":"layers","diff_ids":[]}}`))
						configs = append(configs, config)
						raw, err := json.Marshal(v1.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: manifest.DockerV2Schema2MediaType, Config: config, Layers: []v1.Descriptor{}})
						if err != nil {
							t.Fatal(err)
						}
						child := push(manifest.DockerV2Schema2MediaType, raw)
						child.Platform = &v1.Platform{OS: "linux", Architecture: arch}
						children = append(children, child)
					}
					root := children[0]
					if kind != "single" {
						media := manifest.DockerV2ListMediaType
						if kind == "oci-index" {
							media = v1.MediaTypeImageIndex
						}
						raw, err := json.Marshal(v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: media, Manifests: children})
						if err != nil {
							t.Fatal(err)
						}
						root = push(media, raw)
						if kind == "nested-index" {
							nested := root
							raw, err := json.Marshal(v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageIndex, Manifests: []v1.Descriptor{nested}})
							if err != nil {
								t.Fatal(err)
							}
							root = push(v1.MediaTypeImageIndex, raw)
							children = append(children, nested)
						}
					}
					if err := localstore.CopyGraphLayout(ctx, sourcePath, source, root); err != nil {
						t.Fatal(err)
					}
					root.Annotations = map[string]string{v1.AnnotationRefName: "localhost/archive-test:latest"}
					output := filepath.Join(t.TempDir(), "output")
					var progress bytes.Buffer
					opts := Options{ArchiveReference: "localhost/archive-test:latest", ProgressWriter: &progress}
					if convert {
						opts.ArchiveUncompressed = new(false)
					}
					rejectPath := filepath.Join(t.TempDir(), "policy.json")
					if err := os.WriteFile(rejectPath, []byte(`{"default":[{"type":"reject"}]}`), 0o600); err != nil {
						t.Fatal(err)
					}
					rejected := opts
					rejected.SignaturePolicyPath = rejectPath
					rejectOutput := filepath.Join(t.TempDir(), "rejected")
					if _, err := copyImageArchive(ctx, sourcePath, root, Destination{Transport: transport, Name: rejectOutput}, rejected, nil); err == nil {
						t.Fatal("archive copy ignored signature rejection policy")
					}
					if !convert && (kind == "docker-index" || kind == "nested-index") {
						if _, err := os.Stat(rejectOutput); !os.IsNotExist(err) {
							t.Fatalf("exact graph copy wrote output before policy approval: %v", err)
						}
					}
					if _, err := copyImageArchive(ctx, sourcePath, root, Destination{Transport: transport, Name: output}, opts, nil); err != nil {
						t.Fatal(err)
					}
					var saved content.ReadOnlyStorage
					var resolved v1.Descriptor
					if transport == "oci-dir" {
						store, err := orasoci.NewWithContext(ctx, output)
						if err != nil {
							t.Fatal(err)
						}
						saved = store
						resolved, err = store.Resolve(ctx, "localhost/archive-test:latest")
						if err != nil {
							t.Fatal(err)
						}
					} else {
						store, err := orasoci.NewFromTar(ctx, output)
						if err != nil {
							t.Fatal(err)
						}
						saved = store
						resolved, err = store.Resolve(ctx, "localhost/archive-test:latest")
						if err != nil {
							t.Fatal(err)
						}
					}
					if !strings.Contains(progress.String(), "Writing manifest") {
						t.Fatalf("missing truthful progress: %q", &progress)
					}
					if !convert {
						if resolved.Digest != root.Digest || resolved.MediaType != root.MediaType {
							t.Fatalf("root changed: %+v -> %+v", root, resolved)
						}
						descriptors := append(configs, children...)
						descriptors = append(descriptors, root)
						for _, desc := range descriptors {
							if kind == "single" && (desc.Digest == configs[1].Digest || desc.Digest == children[1].Digest) {
								continue
							}
							raw, err := content.FetchAll(ctx, saved, desc)
							if err != nil || digest.FromBytes(raw) != desc.Digest {
								t.Fatalf("lost exact graph blob %s: %v", desc.Digest, err)
							}
						}
					} else if resolved.MediaType != v1.MediaTypeImageManifest && resolved.MediaType != v1.MediaTypeImageIndex {
						t.Fatalf("explicit conversion retained Docker media: %s", resolved.MediaType)
					}
				})
			}
		}
	}
}
