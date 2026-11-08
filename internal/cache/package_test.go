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

	"oras.land/oras-go/v2/content"

	"github.com/strio-dev/coopr/internal/oci"

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
	resolver, err := oci.NewResolver(oci.Options{TLSVerify: new(false)})
	if err != nil {
		t.Fatal(err)
	}
	remoteStage, localStage := t.TempDir(), t.TempDir()
	remote, err := NewRegistryStore(resolver, strings.TrimPrefix(server.URL, "http://")+"/coopr/package-cache", remoteStage, nil)
	if err != nil {
		t.Fatal(err)
	}
	local, err := NewLocalStore(ctx, filepath.Join(t.TempDir(), "layout"), localStage, nil)
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
	if err != nil || !content.Equal(repeated, root) {
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
	if !content.Equal(got.Artifact, root) {
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
	if seeded.Stage != record.Stage || !content.Equal(seeded.Descriptor, record.Descriptor) || !bytes.Equal(seeded.Config, record.Config) {
		t.Fatalf("seeded package record = %+v, want %+v", seeded, record)
	}

	for _, corrupt := range []bool{false, true} {
		t.Run(map[bool]string{false: "valid", true: "corrupt"}[corrupt], func(t *testing.T) {
			backing, release, err := local.operationTarget(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			target := &graphLookupTarget{
				Target: backing, alter: record.Descriptor.Digest,
				fetches: map[digest.Digest]int{}, read: map[digest.Digest]int64{},
				closeFailure: errGraphLookupClose,
			}
			if corrupt {
				target.failure = "corrupt"
			}
			staging := t.TempDir()
			lookup := &OCIStore{target: target, stagingDir: staging}
			got, path, err := lookup.LookupPackage(ctx, key)
			if !errors.Is(err, errGraphLookupClose) || got != nil || path != "" {
				t.Fatalf("failed payload close admitted result: record=%+v path=%q err=%v", got, path, err)
			}
			if corrupt && !errors.Is(err, content.ErrMismatchedDigest) {
				t.Fatalf("payload verification error lost on close failure: %v", err)
			}
			if target.opened != target.closed || target.fetches[record.Descriptor.Digest] != 1 {
				t.Fatalf("payload reread or leaked streams: fetches=%v opened=%d closed=%d", target.fetches, target.opened, target.closed)
			}
			entries, err := os.ReadDir(staging)
			if err != nil || len(entries) != 0 {
				t.Fatalf("failed payload retained staging data: entries=%v err=%v", entries, err)
			}
		})
	}
}
