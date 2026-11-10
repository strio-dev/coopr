package buildah

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"

	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	upstream "go.podman.io/buildah"
)

func TestArtifactRunViewVerifiesRawFilesAndCleansAfterFailure(t *testing.T) {
	ctx := context.Background()
	store, system := artifactSourceTestStore(t)
	const reference = "registry.example/team/run-files:latest"
	artifactSourceTestAdd(t, ctx, store, reference, map[string]string{"payload.tar": "raw archive bytes", "message": "hello"})
	selected, err := resolveStoredArtifactFileSource(ctx, reference, v1.Platform{OS: "linux", Architecture: runtime.GOARCH}, store, system)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name        string
		properties  map[string]string
		wantOptions []string
	}{
		{
			name:        "defaults",
			wantOptions: []string{"ro", "rbind", "Z", "rprivate"},
		},
		{
			name:        "restrictions",
			properties:  map[string]string{"noexec": "TRUE", "nosuid": "1", "nodev": "T"},
			wantOptions: []string{"ro", "rbind", "Z", "nosuid", "nodev", "noexec", "rprivate"},
		},
		{
			name:        "native shared relabel",
			properties:  map[string]string{"relabel": "shared", "bind-nonrecursive": "True", "bind-propagation": "rslave", "U": "1", "no-dereference": "true"},
			wantOptions: []string{"ro", "bind", "z", "no-dereference", "U", "rslave"},
		},
		{
			name:        "explicit private immutable",
			properties:  map[string]string{"Z": "true", "readonly": "false", "private": "TRUE", "bind-nonrecursive": "false", "noexec": "false"},
			wantOptions: []string{"ro", "rbind", "Z", "private"},
		},
	} {
		for _, source := range []string{"", "/", "payload.tar"} {
			t.Run(test.name+"/"+source, func(t *testing.T) {
				var mounted string
				builder := &inspectingRunBuilder{inspect: func(options upstream.RunOptions) error {
					if len(options.RunMounts) != 0 || len(options.Mounts) != 1 {
						t.Fatalf("artifact not lowered as private direct bind: %#v", options)
					}
					mount := options.Mounts[0]
					mounted = mount.Source
					if !slices.Equal(mount.Options, test.wantOptions) {
						t.Fatalf("artifact mount options = %v, want %v", mount.Options, test.wantOptions)
					}
					file := mounted
					if source != "payload.tar" {
						file = filepath.Join(file, "payload.tar")
					}
					data, err := os.ReadFile(file)
					if err != nil || string(data) != "raw archive bytes" {
						t.Fatalf("raw archive changed: %q %v", data, err)
					}
					return context.Canceled
				}}
				operation := Run{Command: []string{"true"}, sourceStore: store, Mounts: []RunMount{{Type: "bind", BoundFrom: true, Artifact: &selected, Properties: map[string]string{"from": selected.Selected.Digest.Encoded(), "source": source, "target": "/files"}}}}
				for name, value := range test.properties {
					operation.Mounts[0].Properties[name] = value
				}
				if source == "" {
					delete(operation.Mounts[0].Properties, "source")
				}
				if err := operation.apply(builder, ""); !errors.Is(err, context.Canceled) {
					t.Fatalf("builder outcome lost: %v", err)
				}
				if mounted == "" {
					t.Fatal("RUN never observed artifact view")
				}
				if _, err := os.Stat(mounted); !os.IsNotExist(err) {
					t.Fatalf("artifact file view remains after failed RUN: %v", err)
				}
			})
		}
	}
}

func TestArtifactRunInputIdentityAndPrivatePreparedView(t *testing.T) {
	ctx := context.Background()
	store, system := artifactSourceTestStore(t)
	const reference = "registry.example/team/run-cache:latest"
	artifactSourceTestAdd(t, ctx, store, reference, map[string]string{"message": "first"})
	selected, err := resolveStoredArtifactFileSource(ctx, reference, v1.Platform{OS: "linux", Architecture: runtime.GOARCH}, store, system)
	if err != nil {
		t.Fatal(err)
	}
	run := Run{Command: []string{"true"}, sourceStore: store, Mounts: []RunMount{{Type: "bind", BoundFrom: true, Artifact: &selected, Properties: map[string]string{"from": selected.Selected.Digest.Encoded(), "target": "/files"}}}}
	prepared, err := prepareRunInput(ctx, &recordingBuilder{}, "", nil, run)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = prepared.cleanup() }()
	if len(prepared.resolved) != 1 || prepared.resolved[0].Kind != "artifact-files" || prepared.resolved[0].Identity != selected.Selected.Digest.String() {
		t.Fatalf("artifact identity = %#v", prepared.resolved)
	}
	root := prepared.operation.Mounts[0].artifactRoot
	if root == "" {
		t.Fatal("artifact not verified before cache lookup")
	}
	if _, err := os.Stat(filepath.Join(root, "message")); err != nil {
		t.Fatal(err)
	}
	if err := prepared.cleanup(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("prepared artifact not cleaned: %v", err)
	}
}

func TestRunSelectedPathRejectsTraversal(t *testing.T) {
	for _, source := range []string{"..", "../host", "/../../host", "//../../host"} {
		if err := validateStageMountSource(source); err == nil {
			t.Fatalf("traversal accepted: %q", source)
		}
	}
}
