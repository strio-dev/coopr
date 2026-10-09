package buildah

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"coopr/internal/buildcontext"
	"coopr/internal/definition"
	"coopr/internal/planner"
)

func TestParseTransientRunMountsPreservesNativeBindOptions(t *testing.T) {
	mounts, err := ParseTransientRunMounts([]string{"type=bind,src=.,dst=/src,rw,bind-nonrecursive,nosuid,nodev,noexec,rprivate,relabel=shared,U"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := serializeRunMount(mounts[0], "/context")
	if err != nil {
		t.Fatal(err)
	}
	for _, option := range []string{"bind-nonrecursive", "nosuid", "nodev", "noexec", "rprivate", "relabel=shared", "U", "readwrite", "target=/src"} {
		if !slices.Contains(strings.Split(got, ","), option) {
			t.Fatalf("serialized mount %q missing %s", got, option)
		}
	}
	effective := writableContextBindOptions(mounts[0].Properties)
	for _, option := range []string{"bind", "rw", "z", "nosuid", "nodev", "noexec", "rprivate", "U"} {
		if !slices.Contains(effective, option) {
			t.Fatalf("direct mount %v missing %s", effective, option)
		}
	}
	if slices.Contains(effective, "rbind") {
		t.Fatalf("nonrecursive mount became recursive: %v", effective)
	}
}

func TestParseTransientRunMountsRejectsMissingValuesAndDuplicateType(t *testing.T) {
	for _, spec := range []string{"target", "type=bind,target=/src,source", "type=bind,target=/src,from", "type=bind,type=bind,target=/src"} {
		if _, err := ParseTransientRunMounts([]string{spec}); err == nil {
			t.Errorf("accepted malformed mount %q", spec)
		}
	}
}

func TestParseTransientRunMountsLeavesImplicitCacheIDForGraph(t *testing.T) {
	mounts, err := ParseTransientRunMounts([]string{"type=cache,target=/cache"})
	if err != nil {
		t.Fatal(err)
	}
	if len(mounts) != 1 || mounts[0].Properties["id"] != "" {
		t.Fatalf("parser froze a cache identity: %v", mounts)
	}
}

func TestGlobalMountStageContextAndImageSources(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH=1")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	root := t.TempDir()
	contextDir := filepath.Join(root, "context")
	if err := os.MkdirAll(filepath.Join(contextDir, "assets"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(contextDir, "assets", "value"), []byte("mounted"), 0600); err != nil {
		t.Fatal(err)
	}
	store := StoreOptions{RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs"}
	base := newLiveBusyBoxStorage(t, ctx, root, store)
	tests := []struct {
		name, source, mount, command string
		contexts                     []buildcontext.Spec
	}{
		{name: "stage", source: fmt.Sprintf("from %q as=\"producer\"\ncopy \"assets/value\" \"/value\"\nfrom %q\n", base.reference, base.reference), mount: "type=bind,from=producer,target=/src", command: "cat /src/value >/proof"},
		{name: "context", source: fmt.Sprintf("from %q\n", base.reference), mount: "type=bind,from=assets,target=/src", command: "cat /src/value >/proof", contexts: []buildcontext.Spec{{Name: "assets", Kind: buildcontext.Local, Path: filepath.Join(contextDir, "assets")}}},
		{name: "image", source: fmt.Sprintf("from %q\n", base.reference), mount: "type=bind,from=" + base.reference + ",source=/bin/busybox,target=/tools/busybox", command: "/tools/busybox printf mounted >/proof"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mounts, err := ParseTransientRunMounts([]string{test.mount})
			if err != nil {
				t.Fatal(err)
			}
			def, err := definition.Parse(strings.NewReader(test.source + fmt.Sprintf("run %q network=\"none\"\n", test.command)))
			if err != nil {
				t.Fatal(err)
			}
			for iteration := 0; iteration < 2; iteration++ {
				layout := filepath.Join(root, fmt.Sprintf("%s-%d", test.name, iteration))
				_, err := BuildDefinitionSupervised(ctx, def, planner.Options{Mode: planner.Build, Platform: runtime.GOOS + "/" + runtime.GOARCH, BuildContexts: test.contexts}, SupervisedPlanOptions{Store: store, ContextDir: contextDir, Isolation: "rootless", Runtime: "crun", TransientRunMounts: mounts, Jobs: 2, Output: Output{Path: layout}, Stdout: io.Discard, Stderr: os.Stderr})
				if err != nil {
					t.Fatal(err)
				}
				manifest, _ := readPlanImage(t, layout)
				last := filepath.Join(layout, "blobs", "sha256", manifest.Layers[len(manifest.Layers)-1].Digest.Encoded())
				if got := readLayerFile(t, last, "proof"); got != "mounted" {
					t.Fatalf("proof=%q", got)
				}
			}
		})
	}
}

func TestGlobalWritableContextMountUsesNativeOwnershipAndNoexec(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH=1")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	root := t.TempDir()
	contextDir := filepath.Join(root, "context")
	if err := os.Mkdir(contextDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(contextDir, "probe"), []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
		t.Fatal(err)
	}
	store := StoreOptions{RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs"}
	base := newLiveBusyBoxStorage(t, ctx, root, store)
	mounts, err := ParseTransientRunMounts([]string{"type=bind,target=/src,rw,noexec,nosuid,nodev,U,bind-propagation=rprivate,relabel=shared"})
	if err != nil {
		t.Fatal(err)
	}
	source := fmt.Sprintf(`from %q
user "123:456"
run "if /src/probe >/dev/null 2>&1; then exit 17; fi; printf controlled >/src/generated" network="none"
user "0"
copy "generated" "/proof"
`, base.reference)
	def, err := definition.Parse(strings.NewReader(source))
	if err != nil {
		t.Fatal(err)
	}
	layout := filepath.Join(root, "layout")
	_, err = BuildDefinitionSupervised(ctx, def, planner.Options{Mode: planner.Build, Platform: runtime.GOOS + "/" + runtime.GOARCH}, SupervisedPlanOptions{Store: store, ContextDir: contextDir, TransientRunMounts: mounts, Isolation: "rootless", Runtime: "crun", Output: Output{Path: layout}, Stdout: io.Discard, Stderr: os.Stderr})
	if err != nil {
		t.Fatal(err)
	}
	manifest, _ := readPlanImage(t, layout)
	last := filepath.Join(layout, "blobs", "sha256", manifest.Layers[len(manifest.Layers)-1].Digest.Encoded())
	if got := readLayerFile(t, last, "proof"); got != "controlled" {
		t.Fatalf("proof=%q", got)
	}
	if _, err := os.Stat(filepath.Join(contextDir, "generated")); !os.IsNotExist(err) {
		t.Fatalf("host context mutated: %v", err)
	}
}

func TestGlobalMountsDoNotBecomePublishedComponentInstructions(t *testing.T) {
	def, err := definition.Parse(strings.NewReader(`package as="assets"
copy "value" "/value"
extend
copy "/value" "/value" from="assets"
run "true"
`))
	if err != nil {
		t.Fatal(err)
	}
	mounts, err := ParseTransientRunMounts([]string{"type=bind,target=/flag-input"})
	if err != nil {
		t.Fatal(err)
	}
	publication, err := planner.Create(def, planner.Options{Mode: planner.Publish, TransientRunMounts: TransientMountInstructions(mounts)})
	if err != nil {
		t.Fatal(err)
	}
	for _, instruction := range publication.Component.Definition.Instructions {
		if instruction.Name == "run" && len(instruction.Children) != 0 {
			t.Fatal("CLI mounts leaked into published definition")
		}
	}
	invocation, err := planner.Instantiate(publication.Component, planner.Options{Mode: planner.Invoke, TransientRunMounts: TransientMountInstructions(mounts)})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, stage := range invocation.Stages {
		for _, operation := range stage.Operations {
			if operation.Name == "run" {
				found = true
				if operation.TransientMountCount != 1 || len(operation.Children) != 1 {
					t.Fatalf("global mount missing at invocation: %#v", operation)
				}
			}
		}
	}
	if !found {
		t.Fatal("no invocation RUN")
	}
}
