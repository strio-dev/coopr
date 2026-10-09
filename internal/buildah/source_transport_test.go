package buildah

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"coopr/internal/buildcontext"
	"coopr/internal/definition"
	"coopr/internal/oci"
	"coopr/internal/planner"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	imagecopy "go.podman.io/image/v5/copy"
	"go.podman.io/image/v5/signature"
	"go.podman.io/image/v5/transports/alltransports"
	"go.podman.io/image/v5/types"
	"go.podman.io/storage"
	orasoci "oras.land/oras-go/v2/content/oci"
)

func TestNormalizeBaseSource(t *testing.T) {
	root := t.TempDir()
	for _, tc := range []struct {
		input, want string
		transport   bool
	}{
		{"oci:out:latest", "oci:/context/out:latest", true},
		{"oci:../out:latest", "oci:/context/out:latest", true},
		{"oci:/out:latest", "oci:/context/out:latest", true},
		{"oci-archive:image.tar", "oci-archive:/context/image.tar", true},
		{"docker-archive:image.tar:repo:tag", "docker-archive:/context/image.tar:repo:tag", true},
		{"dir:out", "dir:/context/out", true},
		{"docker://ubuntu:latest", "ubuntu:latest", false},
		{"docker:latest", "docker:latest", false},
		{"ubuntu:latest", "ubuntu:latest", false},
	} {
		got, transport, err := normalizeBaseSource(tc.input, root)
		if err != nil || got != strings.ReplaceAll(tc.want, "/context", root) || transport != tc.transport {
			t.Errorf("%s = %q,%v,%v", tc.input, got, transport, err)
		}
	}
}

func TestBaseSourcePolicyUsesUpstreamDecisions(t *testing.T) {
	for _, tc := range []struct {
		action, want string
		fail         bool
	}{
		{"ALLOW", "ubuntu:latest", false},
		{"DENY", "", true},
		{"CONVERT", "registry.example/base@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", false},
	} {
		policy := filepath.Join(t.TempDir(), "source.json")
		data := `{"rules":[{"action":"` + tc.action + `","selector":{"identifier":"docker-image://docker.io/library/ubuntu:latest"},"updates":{"identifier":"docker-image://registry.example/base@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}]}`
		if err := os.WriteFile(policy, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		got, err := policyBaseSource("ubuntu:latest", policy)
		if (err != nil) != tc.fail || got != tc.want {
			t.Fatalf("%s = %q,%v", tc.action, got, err)
		}
	}
}

func TestResolveOCITransportKeepsConfigAndManifest(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	layout := filepath.Join(root, "out")
	source, err := orasoci.NewWithContext(ctx, layout)
	if err != nil {
		t.Fatal(err)
	}
	platform := v1.Platform{OS: "linux", Architecture: "amd64"}
	expected, _ := sourceTestImage(t, ctx, source, platform, "transport")
	if err := source.Tag(ctx, expected, "latest"); err != nil {
		t.Fatal(err)
	}
	store, err := storage.GetStore(storage.StoreOptions{GraphDriverName: "vfs", GraphRoot: filepath.Join(root, "graph"), RunRoot: filepath.Join(root, "run")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := store.Shutdown(true); err != nil {
			t.Error(err)
		}
	})
	policy := filepath.Join(root, "policy.json")
	if err := os.WriteFile(policy, []byte(`{"default":[{"type":"insecureAcceptAnything"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	resolver, err := oci.NewResolver(oci.Options{})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := resolveBaseSource(ctx, resolver, "oci:out:latest", platform, store, &types.SystemContext{SignaturePolicyPath: policy}, root, "")
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Selected.Digest != expected.Digest || len(resolved.ConfigData) == 0 || resolved.Remote {
		t.Fatalf("wrong selection: %+v", resolved)
	}
	if name, err := sourceBaseName(store, resolved); err != nil || name != "" {
		t.Fatalf("filesystem transport invented Docker base name %q: %v", name, err)
	}
	const namedStorageReference = "registry.invalid/transport:latest"
	if err := store.SetNames(resolved.ImageID, []string{namedStorageReference}); err != nil {
		t.Fatal(err)
	}
	byName, err := resolveBaseSource(ctx, resolver, "containers-storage:"+namedStorageReference, platform, store, nil, root, "")
	if err != nil || byName.ImageID != resolved.ImageID || byName.Selected.Digest != expected.Digest {
		t.Fatalf("native named transport did not pin immutable image: %+v, %v", byName, err)
	}
	stored, err := resolveBaseSource(ctx, resolver, "containers-storage:"+resolved.ImageID, platform, store, nil, root, "")
	if err != nil || stored.Selected.Digest != expected.Digest {
		t.Fatalf("native transport = %+v,%v", stored, err)
	}
	if name, err := sourceBaseName(store, stored); err != nil || name != namedStorageReference {
		t.Fatalf("native ID transport lost stored Docker name %q: %v", name, err)
	}
}

func TestFilesystemBaseRejectsEscapingLinks(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "manifest.json"), []byte("host-secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{"dir:escape", "oci:escape"} {
		normalized, _, err := normalizeBaseSource(ref, root)
		if err != nil {
			continue
		}
		frozen, cleanup, err := sanitizeTransportSource(context.Background(), normalized, root)
		if err == nil {
			defer cleanup()
			_, dir, _ := strings.Cut(frozen, ":")
			data, _ := os.ReadFile(filepath.Join(dir, "manifest.json"))
			if string(data) == "host-secret" {
				t.Fatalf("%s read outside context", ref)
			}
		}
	}
}

func TestSourcePolicyValidation(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.json")
	if err := validateSourcePolicy(missing); err == nil {
		t.Fatal("accepted missing policy")
	}
	for _, data := range []string{"{", `{"rules":[{"action":"UNKNOWN","selector":{"identifier":"*"}}]}`} {
		file := filepath.Join(t.TempDir(), "policy.json")
		if err := os.WriteFile(file, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if err := validateSourcePolicy(file); err == nil {
			t.Fatalf("accepted invalid policy %s", data)
		}
	}
}

func TestSourcePolicyAppliesAfterNamedImageContext(t *testing.T) {
	root := t.TempDir()
	policy := filepath.Join(root, "source-policy.json")
	if err := os.WriteFile(policy, []byte(`{"rules":[{"action":"DENY","selector":{"identifier":"docker-image://registry.invalid/base:latest"}}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	def, err := definition.Parse(strings.NewReader(`from "tools"`))
	if err != nil {
		t.Fatal(err)
	}
	options := planner.Options{Mode: planner.Build, BuildContexts: []buildcontext.Spec{{Name: "tools", Kind: buildcontext.DockerImage, Reference: "registry.invalid/base:latest"}}}
	_, _, _, err = planDefinitionInWorker(context.Background(), planWorkerRequest{Definition: def, PlannerOptions: &options, SourcePolicyFile: policy, Store: cacheTestStore(root), ResultPath: filepath.Join(root, "result.json")})
	if err == nil || !strings.Contains(err.Error(), "denied") {
		t.Fatalf("named image context ignored policy: %v", err)
	}
}

func TestResolveFilesystemTransportVariants(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	layout := filepath.Join(root, "input")
	source, err := orasoci.NewWithContext(ctx, layout)
	if err != nil {
		t.Fatal(err)
	}
	platform := v1.Platform{OS: "linux", Architecture: "amd64"}
	expected, config := sourceTestImage(t, ctx, source, platform, "archives")
	if err := source.Tag(ctx, expected, "latest"); err != nil {
		t.Fatal(err)
	}
	src, err := alltransports.ParseImageName("oci:" + layout + ":latest")
	if err != nil {
		t.Fatal(err)
	}
	policy, err := signature.NewPolicyContext(&signature.Policy{Default: []signature.PolicyRequirement{signature.NewPRInsecureAcceptAnything()}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := policy.Destroy(); err != nil {
			t.Error(err)
		}
	})
	policyFile := filepath.Join(root, "policy.json")
	if err := os.WriteFile(policyFile, []byte(`{"default":[{"type":"insecureAcceptAnything"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	store, err := storage.GetStore(storage.StoreOptions{GraphDriverName: "vfs", GraphRoot: filepath.Join(root, "graph"), RunRoot: filepath.Join(root, "run")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := store.Shutdown(true); err != nil {
			t.Error(err)
		}
	})
	resolver, err := oci.NewResolver(oci.Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, transport := range []string{"oci-archive", "docker-archive", "dir"} {
		t.Run(transport, func(t *testing.T) {
			name := transport + "-source"
			destination, err := alltransports.ParseImageName(transport + ":" + filepath.Join(root, name))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := imagecopy.Image(ctx, policy, destination, src, &imagecopy.Options{SourceCtx: &types.SystemContext{BigFilesTemporaryDir: t.TempDir()}, DestinationCtx: &types.SystemContext{BigFilesTemporaryDir: t.TempDir()}}); err != nil {
				t.Fatal(err)
			}
			selected, err := resolveBaseSource(ctx, resolver, transport+":"+name, platform, store, &types.SystemContext{SignaturePolicyPath: policyFile, BigFilesTemporaryDir: t.TempDir()}, root, "")
			if err != nil {
				t.Fatal(err)
			}
			if len(selected.ConfigData) == 0 {
				t.Fatal("configuration lost")
			}
			if selected.ImageID != config.Digest.Encoded() {
				t.Fatalf("configuration changed: %s != %s", selected.ImageID, config.Digest.Encoded())
			}
		})
	}
}

func TestSourcePolicySnapshotSurvivesFileMutation(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "source-policy.json")
	original := `{"rules":[{"action":"DENY","selector":{"identifier":"docker-image://registry.invalid/base:latest"}}]}`
	if err := os.WriteFile(file, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	policy, err := loadSourcePolicy(file)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(stageWorkerRequest{SourcePolicyFile: file, SourcePolicy: policy})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(`{"rules":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	var child stageWorkerRequest
	if err := json.Unmarshal(encoded, &child); err != nil {
		t.Fatal(err)
	}
	if _, err := policyImageSource("registry.invalid/base:latest", child.SourcePolicy); err == nil {
		t.Fatal("stage policy changed when host file changed")
	}
	def, err := definition.Parse(strings.NewReader(`from "tools"`))
	if err != nil {
		t.Fatal(err)
	}
	options := planner.Options{Mode: planner.Build, BuildContexts: []buildcontext.Spec{{Name: "tools", Kind: buildcontext.DockerImage, Reference: "registry.invalid/base:latest"}}}
	_, _, _, err = planDefinitionInWorker(context.Background(), planWorkerRequest{Definition: def, PlannerOptions: &options, SourcePolicyFile: file, SourcePolicy: child.SourcePolicy, Store: cacheTestStore(root), ResultPath: filepath.Join(root, "result.json")})
	if err == nil || !strings.Contains(err.Error(), "denied") {
		t.Fatalf("planning reloaded mutable host policy: %v", err)
	}
}

func TestSourcePolicyConvertsNamedImageContextToOCI(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	layout, err := orasoci.NewWithContext(ctx, filepath.Join(root, "base"))
	if err != nil {
		t.Fatal(err)
	}
	platform := v1.Platform{OS: "linux", Architecture: "amd64"}
	expected, _ := sourceTestImage(t, ctx, layout, platform, "converted-context")
	if err := layout.Tag(ctx, expected, "latest"); err != nil {
		t.Fatal(err)
	}
	policy := filepath.Join(root, "source-policy.json")
	if err := os.WriteFile(policy, []byte(`{"rules":[{"action":"CONVERT","selector":{"identifier":"docker-image://registry.invalid/base:latest"},"updates":{"identifier":"oci:base:latest"}}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	signaturePolicy := filepath.Join(root, "policy.json")
	if err := os.WriteFile(signaturePolicy, []byte(`{"default":[{"type":"insecureAcceptAnything"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	def, err := definition.Parse(strings.NewReader(`from "tools"`))
	if err != nil {
		t.Fatal(err)
	}
	options := planner.Options{Mode: planner.Build, BuildContexts: []buildcontext.Spec{{Name: "tools", Kind: buildcontext.DockerImage, Reference: "registry.invalid/base:latest"}}}
	plan, _, selected, err := planDefinitionInWorker(ctx, planWorkerRequest{Definition: def, PlannerOptions: &options, SourcePolicyFile: policy, SignaturePolicyPath: signaturePolicy, ContextDir: root, Store: cacheTestStore(root), ResultPath: filepath.Join(root, "result.json")})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Stages) != 1 || !plan.Stages[0].DeferredImageSource || plan.Stages[0].SourceContext != "tools" || len(selected.all) != 0 {
		t.Fatalf("converted filesystem context was eagerly resolved: %+v, %+v", plan.Stages, selected.all)
	}
}

func TestImageSourceDirectoryLinksStayInsideSnapshot(t *testing.T) {
	var source, output bytes.Buffer
	writer := tar.NewWriter(&source)
	for _, header := range []*tar.Header{
		{Name: "blobs", Typeflag: tar.TypeDir, Mode: 0700},
		{Name: "blobs/sha256", Typeflag: tar.TypeDir, Mode: 0700},
		{Name: "alias", Typeflag: tar.TypeSymlink, Linkname: "/blobs/sha256", Mode: 0777},
	} {
		if err := writer.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := filterImageArchive(context.Background(), &source, &output); err != nil {
		t.Fatal(err)
	}
	reader := tar.NewReader(&output)
	for i := 0; i < 3; i++ {
		header, err := reader.Next()
		if err != nil {
			t.Fatal(err)
		}
		if i == 2 && (header.Typeflag != tar.TypeSymlink || header.Linkname != "blobs/sha256") {
			t.Fatalf("unconfined directory link: %+v", header)
		}
	}
}
