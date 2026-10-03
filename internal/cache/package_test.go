package cache

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"coopr/internal/oci"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

func testPackageKey() PackageKey {
	return PackageKey{
		Plan: digest.FromString("selected package closure"), Output: "payload",
		Bases:   map[string]v1.Descriptor{"image-base": oci.Descriptor(v1.MediaTypeImageManifest, []byte("manifest"))},
		Context: digest.FromString("filtered context"), Platform: v1.Platform{OS: "linux", Architecture: "amd64"},
		Executor: "buildah-v1", Frontend: "coopr-definition-v1", Lowering: "coopr-package-v1",
	}
}

func testPackageRecord(key PackageKey, data []byte) PackageRecord {
	descriptor := oci.Descriptor(oci.ComponentPackageType, data)
	config := json.RawMessage(`{"architecture":"amd64","os":"linux","rootfs":{"type":"layers","diff_ids":["` + descriptor.Digest.String() + `"]}}`)
	return PackageRecord{Key: key, Stage: key.Output, Descriptor: descriptor, Config: config}
}

func TestPackageKeyInvalidatesEveryPortableInput(t *testing.T) {
	base := testPackageKey()
	want, err := base.Digest()
	if err != nil {
		t.Fatal(err)
	}
	changes := []func(*PackageKey){
		func(k *PackageKey) { k.Plan = digest.FromString("other plan") },
		func(k *PackageKey) { k.Output = "other" },
		func(k *PackageKey) {
			k.Bases["image-base"] = oci.Descriptor(v1.MediaTypeImageManifest, []byte("other"))
		},
		func(k *PackageKey) { k.Context = digest.FromString("other context") },
		func(k *PackageKey) { k.Platform.Architecture = "arm64" },
		func(k *PackageKey) { k.Executor = "other" },
		func(k *PackageKey) { k.Frontend = "other" },
		func(k *PackageKey) { k.Lowering = "other" },
	}
	for index, change := range changes {
		candidate := base
		candidate.Bases = map[string]v1.Descriptor{"image-base": base.Bases["image-base"]}
		change(&candidate)
		got, err := candidate.Digest()
		if err != nil || got == want {
			t.Fatalf("change %d did not invalidate package key: %s %v", index, got, err)
		}
	}
}

func TestPackageStoreRegistrySeedsIndependentLocalLayout(t *testing.T) {
	ctx := context.Background()
	server := httptest.NewServer(registry.New())
	t.Cleanup(server.Close)
	resolver, err := oci.NewResolver(oci.Options{PlainHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	remoteStage, localStage := t.TempDir(), t.TempDir()
	remote, err := NewRegistryStore(resolver, strings.TrimPrefix(server.URL, "http://")+"/coopr/package-cache", remoteStage)
	if err != nil {
		t.Fatal(err)
	}
	local, err := NewLocalStore(ctx, filepath.Join(t.TempDir(), "layout"), localStage)
	if err != nil {
		t.Fatal(err)
	}
	key := testPackageKey()
	data := []byte("portable completed package tar")
	record := testPackageRecord(key, data)
	source := filepath.Join(remoteStage, "package.tar")
	if err := os.WriteFile(source, data, 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := remote.PutPackage(ctx, key, record, source)
	if err != nil {
		t.Fatal(err)
	}
	repeated, err := remote.PutPackage(ctx, key, record, source)
	if err != nil || !sameDescriptor(repeated, root) {
		t.Fatalf("idempotent package cache publication: descriptor=%+v err=%v", repeated, err)
	}
	if _, _, err := local.LookupPackage(ctx, key); !errors.Is(err, ErrMiss) {
		t.Fatalf("fresh local package cache lookup = %v", err)
	}
	got, downloaded, err := remote.LookupPackage(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Remove(downloaded) }()
	if !sameDescriptor(got.Artifact, root) {
		t.Fatalf("verified package artifact = %+v, want %+v", got.Artifact, root)
	}
	if _, err := local.PutPackage(ctx, key, *got, downloaded); err != nil {
		t.Fatal(err)
	}
	seeded, copied, err := local.LookupPackage(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Remove(copied) }()
	contents, err := os.ReadFile(copied)
	if err != nil || !bytes.Equal(contents, data) {
		t.Fatalf("seeded package bytes = %q, error %v", contents, err)
	}
	if seeded.Stage != record.Stage || !sameDescriptor(seeded.Descriptor, record.Descriptor) || !bytes.Equal(seeded.Config, record.Config) {
		t.Fatalf("seeded package record = %+v, want %+v", seeded, record)
	}
}
