package buildah

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/coopr/internal/oci"
	upstream "go.podman.io/buildah"
	"go.podman.io/buildah/define"
	"go.podman.io/image/v5/types"
	"go.podman.io/storage"
	orasoci "oras.land/oras-go/v2/content/oci"
)

type instructionBuilderStore struct {
	storage.Store
	creates, mounts atomic.Int64
	cancel          context.CancelFunc
}

func (s *instructionBuilderStore) CreateContainer(id string, names []string, image, layer, metadata string, options *storage.ContainerOptions) (*storage.Container, error) {
	s.creates.Add(1)
	container, err := s.Store.CreateContainer(id, names, image, layer, metadata, options)
	if err == nil && s.cancel != nil {
		s.cancel()
	}
	return container, err
}

func (s *instructionBuilderStore) Mount(id, label string) (string, error) {
	s.mounts.Add(1)
	return s.Store.Mount(id, label)
}

func TestInstructionBuilderDefersWorkingContainer(t *testing.T) {
	if testing.Short() {
		t.Skip("requires native containers/storage")
	}
	ctx := t.Context()
	root := t.TempDir()
	lease, err := acquireStore(cacheTestStore(root))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := lease.Close(); err != nil {
			t.Error(err)
		}
	}()
	fixture := newRawDockerFixture(t)
	var config map[string]any
	if err := json.Unmarshal(fixture.configData, &config); err != nil {
		t.Fatal(err)
	}
	config["created"] = "2024-01-02T03:04:05Z"
	config["config"].(map[string]any)["Hostname"] = "metadata-parity"
	configData, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	var manifest v1.Manifest
	if err := json.Unmarshal(fixture.manifestData, &manifest); err != nil {
		t.Fatal(err)
	}
	system := &types.SystemContext{SignaturePolicyPath: writeComponentTestPolicy(t, root), BigFilesTemporaryDir: root}
	variants := map[string]v1.Descriptor{}
	imageID := ""
	for _, format := range []string{define.OCIv1ImageManifest, define.Dockerv2ImageManifest} {
		layoutPath := filepath.Join(t.TempDir(), "layout")
		layout, err := orasoci.New(layoutPath)
		if err != nil {
			t.Fatal(err)
		}
		manifest.MediaType = format
		configType, layerType := v1.MediaTypeImageConfig, v1.MediaTypeImageLayer
		if format == define.Dockerv2ImageManifest {
			configType, layerType = dockerImageConfigMediaType, dockerLayerMediaType
		}
		manifest.Config = oci.Descriptor(configType, configData)
		manifest.Layers[0].MediaType = layerType
		data, err := json.Marshal(manifest)
		if err != nil {
			t.Fatal(err)
		}
		selected := oci.Descriptor(format, data)
		for _, blob := range []struct {
			descriptor v1.Descriptor
			data       []byte
		}{{manifest.Config, configData}, {manifest.Layers[0], fixture.blobs[manifest.Layers[0].Digest]}, {selected, data}} {
			if err := layout.Push(ctx, blob.descriptor, bytes.NewReader(blob.data)); err != nil {
				t.Fatal(err)
			}
		}
		if err := layout.Tag(ctx, selected, "latest"); err != nil {
			t.Fatal(err)
		}
		id, err := ImportSelectedImage(ctx, lease.store, system, layoutPath, selected)
		if err != nil || imageID != "" && id != imageID {
			t.Fatal("variant did not retain the same native image", id, imageID, err)
		}
		imageID, variants[format] = id, selected
	}
	for _, format := range []string{define.OCIv1ImageManifest, define.Dockerv2ImageManifest} {
		t.Run(format, func(t *testing.T) {
			selected := variants[format]
			alias, err := oci.SelectedStoredImage(ctx, lease.store, imageID, selected.Digest)
			if err != nil {
				t.Fatal(err)
			}
			options := upstream.BuilderOptions{FromImage: alias, PullPolicy: define.PullNever, Format: format,
				Isolation: define.IsolationChroot, SystemContext: system, PreserveBaseImageAnns: true,
				CommonBuildOpts: &upstream.CommonBuildOptions{}, Args: map[string]string{"argument": "retained"}}
			reference, err := upstream.NewBuilder(ctx, lease.store, options)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if reference != nil {
					_ = reference.Delete()
				}
			}()
			rootMetadata, err := capturePackageRootMetadata(lease.store, reference)
			if err != nil {
				t.Fatal(err)
			}
			counted := &instructionBuilderStore{Store: lease.store}
			view, err := importInstructionBuilder(ctx, counted, options, imageID, selected.Digest)
			if err != nil {
				t.Fatal(err)
			}
			if counted.creates.Load() != 0 || counted.mounts.Load() != 0 || view.ContainerID != "" || view.MountPoint != "" {
				t.Fatal("metadata import created or mounted a working container")
			}
			if view.FromImageDigest != selected.Digest.String() || !reflect.DeepEqual(view.OCIv1, reference.OCIv1) || !reflect.DeepEqual(view.Docker, reference.Docker) {
				t.Fatal("metadata import changed selected image configuration or history")
			}
			if err := reference.Delete(); err != nil {
				t.Fatal(err)
			}
			reference = nil
			view.SetEnv("DEFERRED", "yes")
			view.SetCmd([]string{"/proof"})
			view.SetAnnotation("coopr.test.metadata", "retained")
			created := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
			history := v1.History{Created: &created, CreatedBy: "metadata tail", EmptyLayer: true}
			view.OCIv1.History = append(view.OCIv1.History, history)
			view.Docker.History = append(view.Docker.History, dockerHistory([]v1.History{history})...)
			view.PrependedEmptyLayers = []v1.History{history}
			view.AppendedEmptyLayers = []v1.History{history}
			view.PrependedLinkedLayers = []upstream.LinkedLayer{{History: history, BlobPath: t.TempDir()}}
			view.AppendedLinkedLayers = []upstream.LinkedLayer{{History: history, BlobPath: t.TempDir()}}
			view.ImageCreatedBy, view.ImageHistoryComment = "next operation", "retained comment"
			working, err := realizeInstructionBuilder(ctx, counted, view, options, rootMetadata)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if working != nil {
					_ = working.Delete()
				}
			}()
			if counted.creates.Load() != 1 || working.ContainerID == "" || working.Isolation != options.Isolation || working.Docker.Config != &working.Docker.ContainerConfig {
				t.Fatal("materialization lost the native working-container state")
			}
			if !reflect.DeepEqual(view.OCIv1, working.OCIv1) || !reflect.DeepEqual(view.Docker, working.Docker) ||
				!reflect.DeepEqual(view.Annotations(), working.Annotations()) || !reflect.DeepEqual(view.Args, working.Args) ||
				!reflect.DeepEqual(view.PrependedEmptyLayers, working.PrependedEmptyLayers) || !reflect.DeepEqual(view.AppendedEmptyLayers, working.AppendedEmptyLayers) ||
				!reflect.DeepEqual(view.PrependedLinkedLayers, working.PrependedLinkedLayers) || !reflect.DeepEqual(view.AppendedLinkedLayers, working.AppendedLinkedLayers) ||
				view.ImageCreatedBy != working.ImageCreatedBy || view.ImageHistoryComment != working.ImageHistoryComment {
				t.Fatal("materialization discarded deferred image metadata")
			}
			if got, err := capturePackageRootMetadata(counted, working); err != nil || !reflect.DeepEqual(got, rootMetadata) {
				t.Fatal("materialization changed root metadata", got, rootMetadata, err)
			}
			input := filepath.Join(t.TempDir(), "input")
			if err := os.WriteFile(input, []byte("fresh filesystem operation"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := working.Add("/fresh", false, upstream.AddAndCopyOptions{}, input); err != nil {
				t.Fatal(err)
			}
			mount, err := working.Mount(working.MountLabel)
			if err != nil {
				t.Fatal(err)
			}
			data, readErr := os.ReadFile(filepath.Join(mount, "fresh"))
			if err := errors.Join(readErr, working.Unmount()); err != nil || string(data) != "fresh filesystem operation" {
				t.Fatal("subsequent filesystem operation lost its content", string(data), err)
			}
			if err := working.Delete(); err != nil {
				t.Fatal(err)
			}
			working = nil
			for _, failure := range []string{"cancel", "invalid root"} {
				t.Run(failure, func(t *testing.T) {
					cancelCtx, cancel := context.WithCancel(ctx)
					defer cancel()
					metadata := rootMetadata
					if failure == "cancel" {
						counted.cancel = cancel
					} else {
						counted.cancel = nil
						metadata = &PackageRootMetadata{Mode: -1}
					}
					got, err := realizeInstructionBuilder(cancelCtx, counted, view, options, metadata)
					if err == nil || got != nil || failure == "cancel" && !errors.Is(err, context.Canceled) {
						t.Fatal("failed materialization returned a working builder", got, err)
					}
					containers, err := lease.store.Containers()
					if err != nil || len(containers) != 0 {
						t.Fatal("failed materialization leaked a container", containers, err)
					}
				})
			}
		})
	}
}
