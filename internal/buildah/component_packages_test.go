package buildah

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"coopr/internal/definition"
	"coopr/internal/imageconfig"

	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"go.podman.io/image/v5/types"
	"go.podman.io/storage"
)

func TestComponentPackageImporterRejectsInvalidRequests(t *testing.T) {
	root := t.TempDir()
	store, err := storage.GetStore(storage.StoreOptions{GraphDriverName: "vfs", GraphRoot: filepath.Join(root, "graph"), RunRoot: filepath.Join(root, "run")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = store.Shutdown(true) })
	resolver, _, _ := localComponentResolver(t, context.Background(), v1.Platform{OS: "linux", Architecture: runtime.GOARCH})
	if _, err := NewComponentPackageImporter(nil, store, nil, root); err == nil || !strings.Contains(err.Error(), "resolver") {
		t.Fatalf("nil resolver error = %v", err)
	}
	if _, err := NewComponentPackageImporter(resolver, nil, nil, root); err == nil || !strings.Contains(err.Error(), "store") {
		t.Fatalf("nil store error = %v", err)
	}
	if _, err := NewComponentPackageImporter(resolver, store, nil, "relative"); err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("relative temporary directory error = %v", err)
	}
	importer, err := NewComponentPackageImporter(resolver, store, nil, root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := importer.Import(context.Background(), nil, "0", v1.Platform{OS: "linux", Architecture: runtime.GOARCH}); err == nil || !strings.Contains(err.Error(), "artifact") {
		t.Fatalf("nil plan error = %v", err)
	}
}

func TestComponentPackageImporterMemoizesAndClonesConfig(t *testing.T) {
	ctx := context.Background()
	platform := v1.Platform{OS: "linux", Architecture: runtime.GOARCH}
	resolver, _, _ := localComponentResolver(t, ctx, platform)
	resolved, err := ResolveComponentPlan(ctx, ComponentPlanRequest{
		Resolver: resolver, Reference: "local:tool", Parameters: map[string]string{"channel": "stable"}, Platform: platform,
	})
	if err != nil {
		t.Fatal(err)
	}
	pkg := resolved.PackageInputs["0"]
	key, err := componentPackageCacheKey(resolved.Identity, pkg, platform)
	if err != nil {
		t.Fatal(err)
	}
	config, err := imageconfig.Parse(pkg.Config)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	close(done)
	manifestDigest := digest.FromString("package-manifest")
	importer := &ComponentPackageImporter{resolver: resolver, imports: map[componentPackageKey]*componentPackageImport{
		key: {done: done, result: ImportedComponentPackage{ImageID: "imported-image", ManifestDigest: manifestDigest, Config: config}},
	}}
	first, err := importer.Import(ctx, resolved, "0", platform)
	if err != nil {
		t.Fatal(err)
	}
	if first.ImageID == "" || first.ManifestDigest != manifestDigest || first.Config == nil || len(importer.imports) != 1 {
		t.Fatalf("first import = %+v, cache entries=%d", first, len(importer.imports))
	}
	if err := first.Config.Apply(definition.Instruction{Name: "env", Properties: map[string]string{"MUTATED": "yes"}}); err != nil {
		t.Fatal(err)
	}
	second, err := importer.Import(ctx, resolved, "0", platform)
	if err != nil {
		t.Fatal(err)
	}
	if second.ImageID != first.ImageID || second.ManifestDigest != manifestDigest || len(importer.imports) != 1 {
		t.Fatalf("memoized import image=%q entries=%d, want %q and 1", second.ImageID, len(importer.imports), first.ImageID)
	}
	secondConfig, err := second.Config.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(secondConfig, []byte("MUTATED")) {
		t.Fatalf("memoized config aliases caller mutation: %s", secondConfig)
	}
}

func TestComponentPackageImporterConfigDigestSeparatesMemoEntries(t *testing.T) {
	ctx := context.Background()
	platform := v1.Platform{OS: "linux", Architecture: runtime.GOARCH}
	resolver, _, _ := localComponentResolver(t, ctx, platform)
	resolved, err := ResolveComponentPlan(ctx, ComponentPlanRequest{
		Resolver: resolver, Reference: "local:tool", Parameters: map[string]string{"channel": "stable"}, Platform: platform,
	})
	if err != nil {
		t.Fatal(err)
	}
	pkg := resolved.PackageInputs["0"]
	first, err := componentPackageCacheKey(resolved.Identity, pkg, platform)
	if err != nil {
		t.Fatal(err)
	}
	var config map[string]any
	if err := json.Unmarshal(pkg.Config, &config); err != nil {
		t.Fatal(err)
	}
	config["coopr.test"] = map[string]any{"preserved": true}
	pkg.Config, err = json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	second, err := componentPackageCacheKey(resolved.Identity, pkg, platform)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("different package config digest reused a memo key")
	}
}

func TestComponentPackageImporterLiveRootless(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping a live rootless package importer in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	platform := v1.Platform{OS: "linux", Architecture: runtime.GOARCH}
	resolver, _, _ := localComponentResolver(t, ctx, platform)
	resolved, err := ResolveComponentPlan(ctx, ComponentPlanRequest{
		Resolver: resolver, Reference: "local:tool", Parameters: map[string]string{"channel": "stable"}, Platform: platform,
	})
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	store, err := storage.GetStore(storage.StoreOptions{GraphDriverName: "vfs", GraphRoot: filepath.Join(root, "graph"), RunRoot: filepath.Join(root, "run")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = store.Shutdown(true) })
	policy := filepath.Join(root, "policy.json")
	if err := os.WriteFile(policy, []byte(`{"default":[{"type":"insecureAcceptAnything"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	importer, err := NewComponentPackageImporter(resolver, store, &types.SystemContext{SignaturePolicyPath: policy, BigFilesTemporaryDir: root}, root)
	if err != nil {
		t.Fatal(err)
	}
	imported, err := importer.Import(ctx, resolved, "0", platform)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := packageImageConfig(ctx, store, imported.ImageID, &types.SystemContext{SignaturePolicyPath: policy, BigFilesTemporaryDir: root})
	if err != nil {
		t.Fatal(err)
	}
	want, err := imported.Config.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	pkg := resolved.PackageInputs["0"]
	if digest.FromBytes(stored) != digest.FromBytes(pkg.Config) {
		t.Fatalf("stored config digest %s, published digest %s", digest.FromBytes(stored), digest.FromBytes(pkg.Config))
	}
	var storedConfig, sidecarConfig any
	if err := json.Unmarshal(stored, &storedConfig); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(want, &sidecarConfig); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(storedConfig, sidecarConfig) {
		t.Fatalf("stored config and sidecar differ:\n%s\n%s", stored, want)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "coopr-component-package-") {
			t.Fatalf("temporary package download leaked: %s", entry.Name())
		}
	}
}
