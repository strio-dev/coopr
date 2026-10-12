package transfer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"coopr/internal/buildah"
	"coopr/internal/imagestore"
	"coopr/internal/oci"
	"github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"go.podman.io/common/libimage/manifests"
	"go.podman.io/image/v5/transports/alltransports"
	"go.podman.io/image/v5/types"
	"go.podman.io/storage"
)

func signedIndexFixture(t *testing.T, store buildah.StoreOptions, marker string) (v1.Descriptor, []byte, map[string]oci.StoredSelection) {
	t.Helper()
	selections := map[string]oci.StoredSelection{}
	var members []v1.Descriptor
	for _, arch := range []string{"amd64", "arm64"} {
		platform := v1.Platform{OS: "linux", Architecture: arch}
		selected := nativeEmptyImageFixture(t, store, platform, marker+arch)
		selections["linux/"+arch] = selected
		member := selected.Manifest
		member.Platform = &platform
		member.Annotations = map[string]string{"member": marker + arch}
		members = append(members, member)
	}
	// Repeated descriptors are legal: transporting a graph must not turn it into
	// a platform-keyed list and erase descriptor metadata or repeated entries.
	duplicate := members[0]
	duplicate.Annotations = map[string]string{"member": "duplicate"}
	members = append(members, duplicate)
	raw, err := json.Marshal(v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageIndex, Manifests: members, Annotations: map[string]string{"fixture": marker}})
	if err != nil {
		t.Fatal(err)
	}
	root := oci.Descriptor(v1.MediaTypeImageIndex, raw)
	if err := nativeTestIndex(context.Background(), store, "index:latest", root, raw, selections); err != nil {
		t.Fatal(err)
	}
	return root, raw, selections
}

func TestSignedStoredIndexNativeDestinations(t *testing.T) {
	if testing.Short() {
		t.Skip("requires native storage workers and GPG")
	}
	ctx := context.Background()
	store := nativeTestStore(t.TempDir())
	root, raw, selections := signedIndexFixture(t, store, "signed")
	home, err := os.MkdirTemp("", "coopr-index-gpg-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	t.Setenv("GNUPGHOME", home)
	t.Cleanup(func() { _ = exec.Command("gpgconf", "--homedir", home, "--kill", "gpg-agent").Run() })
	fingerprint, _ := generateGPGKey(t, ctx, home, "Coopr Index Transfer <index@example.invalid>", "")
	wantSignatures := map[digest.Digest][][]byte{}
	sigstore := []byte("\x00sigstore-json\n{\"mimeType\":\"application/vnd.dev.cosign.simplesigning.v1+json\",\"payload\":\"cGF5bG9hZA==\",\"annotations\":{\"coopr.test\":\"preserve\"}}")
	for _, selected := range selections {
		if err := signStoredImage(ctx, selected.ImageID, selected.Manifest.Digest, "localhost/index:latest", Options{BuildStore: store, Signing: SigningOptions{SignBy: fingerprint}}); err != nil {
			t.Fatal(err)
		}
		if err := buildah.WithStore(store, func(backend storage.Store) error {
			state, err := oci.CaptureStoredSignatures(backend, selected.ImageID)
			if err != nil {
				return err
			}
			signatures, err := oci.StoredSignatureBlobs(state, selected.Manifest.Digest, true)
			if err != nil {
				return err
			}
			if len(signatures) == 0 {
				t.Fatal("fixture has no native GPG signature")
			}
			// Keep a second signature format alongside GPG: the legacy public
			// GetSignatures interface filters sigstore signatures out silently.
			signatures = append(signatures, sigstore)
			if err := oci.WriteStoredManifestSignatures(backend, selected.ImageID, selected.Manifest.Digest, true, signatures, state); err != nil {
				return err
			}
			wantSignatures[selected.Manifest.Digest] = signatures
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	// Older/native producers may store signatures only for the default
	// manifest, without a per-digest size vector. This remains a valid source.
	if err := buildah.WithStore(store, func(backend storage.Store) error {
		member := selections["linux/arm64"]
		metadata, err := backend.Metadata(member.ImageID)
		if err != nil {
			return err
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal([]byte(metadata), &fields); err != nil {
			return err
		}
		delete(fields, "signatures-sizes")
		raw, err := json.Marshal(fields)
		if err != nil {
			return err
		}
		return backend.SetMetadata(member.ImageID, string(raw))
	}); err != nil {
		t.Fatal(err)
	}
	var indexID string
	if err := buildah.WithStore(store, func(backend storage.Store) error {
		image, err := backend.Image("localhost/index:latest")
		if err == nil {
			indexID = image.ID
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := signStoredImage(ctx, indexID, root.Digest, "localhost/index:latest", Options{BuildStore: store, Signing: SigningOptions{SignBy: fingerprint}}); err != nil {
		t.Fatal(err)
	}
	if err := buildah.WithStore(store, func(backend storage.Store) error {
		state, err := oci.CaptureStoredSignatures(backend, indexID)
		if err != nil {
			return err
		}
		signatures, err := oci.StoredSignatureBlobs(state, root.Digest, true)
		if err != nil {
			return err
		}
		if len(signatures) == 0 {
			t.Fatal("fixture has no root GPG signature")
		}
		signatures = append(signatures, sigstore)
		if err := oci.WriteStoredManifestSignatures(backend, indexID, root.Digest, true, signatures, state); err != nil {
			return err
		}
		wantSignatures[root.Digest] = signatures
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// The index still selects the original manifest after its native record's
	// default switches to another independently signed representation.
	selected := selections["linux/amd64"]
	var alternate v1.Descriptor
	if err := buildah.WithStore(store, func(backend storage.Store) error {
		data, err := backend.ImageBigData(selected.ImageID, storage.ImageDigestBigDataKey)
		if err != nil {
			return err
		}
		var manifest v1.Manifest
		if err := json.Unmarshal(data, &manifest); err != nil {
			return err
		}
		manifest.Annotations = map[string]string{"representation": "alternate-default"}
		data, err = json.Marshal(manifest)
		if err != nil {
			return err
		}
		alternate = oci.Descriptor(v1.MediaTypeImageManifest, data)
		if err := backend.SetImageBigData(selected.ImageID, storage.ImageDigestManifestBigDataNamePrefix+"-"+alternate.Digest.String(), data, func([]byte) (digest.Digest, error) { return alternate.Digest, nil }); err != nil {
			return err
		}
		_, err = imagestore.FromStore(backend).TagSelected(ctx, selected.ImageID, alternate, "alternate:latest")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := signStoredImage(ctx, selected.ImageID, alternate.Digest, "localhost/alternate:latest", Options{BuildStore: store, Signing: SigningOptions{SignBy: fingerprint}}); err != nil {
		t.Fatal(err)
	}
	for _, remove := range []bool{false, true} {
		t.Run(map[bool]string{false: "preserve", true: "remove"}[remove], func(t *testing.T) {
			dst := Destination{Transport: "dir", Name: filepath.Join(t.TempDir(), "output")}
			result, err := Copy(ctx, oci.Image, "index:latest", dst, Options{BuildStore: store, Push: PushOptions{RemoveSignatures: remove}})
			if err != nil {
				t.Fatal(err)
			}
			if result != dst.String()+"@"+root.Digest.String() {
				t.Fatalf("result %q does not identify selected root %s", result, root.Digest)
			}
			ref, err := alltransports.ParseImageName(dst.String())
			if err != nil {
				t.Fatal(err)
			}
			src, err := ref.NewImageSource(ctx, &types.SystemContext{})
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := src.Close(); err != nil {
					t.Error(err)
				}
			}()
			got, _, err := src.GetManifest(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, raw) {
				t.Fatalf("index metadata or duplicate members changed: %s", got)
			}
			for member, want := range wantSignatures {
				// Read the transport's encoded sidecars, rather than its public
				// compatibility API which exposes only simple-signing signatures.
				prefix := member.Encoded() + "."
				if member == root.Digest {
					prefix = ""
				}
				paths, err := filepath.Glob(filepath.Join(dst.Name, prefix+"signature-*"))
				if err != nil {
					t.Fatal(err)
				}
				if remove {
					if len(paths) != 0 {
						t.Fatalf("signature removal retained %d sidecars", len(paths))
					}
					continue
				}
				if len(paths) != len(want) {
					t.Fatalf("member %s signature sidecars = %d, want %d", member, len(paths), len(want))
				}
				for i, blob := range want {
					got, err := os.ReadFile(filepath.Join(dst.Name, fmt.Sprintf("%ssignature-%d", prefix, i+1)))
					if err != nil {
						t.Fatal(err)
					}
					if !bytes.Equal(got, blob) {
						t.Fatalf("member %s encoded signature %d changed", member, i+1)
					}
				}
			}
		})
	}
	t.Run("oci-unsupported-signatures", func(t *testing.T) {
		dst := Destination{Transport: "oci", Name: filepath.Join(t.TempDir(), "output") + ":app"}
		if _, err := Copy(ctx, oci.Image, "index:latest", dst, Options{BuildStore: store}); err == nil || !strings.Contains(strings.ToLower(err.Error()), "signatur") {
			t.Fatalf("signed OCI destination should report its upstream signature restriction: %v", err)
		}
		dst.Name = filepath.Join(t.TempDir(), "unsigned") + ":app"
		if _, err := Copy(ctx, oci.Image, "index:latest", dst, Options{BuildStore: store, Push: PushOptions{RemoveSignatures: true}}); err != nil {
			t.Fatal(err)
		}
	})
}

func TestStoredIndexPushHonorsNativePolicyAndImmutableRoot(t *testing.T) {
	if testing.Short() {
		t.Skip("requires native storage workers")
	}
	ctx := context.Background()
	store := nativeTestStore(t.TempDir())
	root, raw, _ := signedIndexFixture(t, store, "old")
	var oldID string
	if err := buildah.WithStore(store, func(backend storage.Store) error {
		image, err := backend.Image("localhost/index:latest")
		if err == nil {
			oldID = image.ID
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	home, err := os.MkdirTemp("", "coopr-index-gpg-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	t.Setenv("GNUPGHOME", home)
	t.Cleanup(func() { _ = exec.Command("gpgconf", "--homedir", home, "--kill", "gpg-agent").Run() })
	fingerprint, _ := generateGPGKey(t, ctx, home, "Coopr Historical Index <historical@example.invalid>", "")
	if err := signStoredImage(ctx, oldID, root.Digest, "localhost/index:latest", Options{BuildStore: store, Signing: SigningOptions{SignBy: fingerprint}}); err != nil {
		t.Fatal(err)
	}
	var wantRootSignatures [][]byte
	if err := buildah.WithStore(store, func(backend storage.Store) error {
		state, err := oci.CaptureStoredSignatures(backend, oldID)
		if err != nil {
			return err
		}
		wantRootSignatures, err = oci.StoredSignatureBlobs(state, root.Digest, true)
		if err != nil {
			return err
		}
		if len(wantRootSignatures) == 0 {
			t.Fatal("old index has no GPG signature")
		}
		wantRootSignatures = append(wantRootSignatures, []byte("\x00sigstore-json\n{\"mimeType\":\"application/vnd.dev.cosign.simplesigning.v1+json\",\"payload\":\"cGF5bG9hZA==\",\"annotations\":{\"coopr.test\":\"historical\"}}"))
		return oci.WriteStoredManifestSignatures(backend, oldID, root.Digest, true, wantRootSignatures, state)
	}); err != nil {
		t.Fatal(err)
	}
	current, currentRaw, currentSelections := signedIndexFixture(t, store, "current")
	if err := buildah.WithStore(store, func(backend storage.Store) error {
		ids := map[digest.Digest]string{}
		for _, selected := range currentSelections {
			ids[selected.Manifest.Digest] = selected.ImageID
		}
		lock, err := manifests.LockerForImage(backend, oldID)
		if err != nil {
			return err
		}
		lock.Lock()
		defer lock.Unlock()
		_, err = imagestore.FromStore(backend).UpdateStoredIndex(ctx, oldID, current, currentRaw, ids, "index:latest")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := buildah.WithStore(store, func(backend storage.Store) error {
		state, err := oci.CaptureStoredSignatures(backend, oldID)
		if err != nil {
			return err
		}
		retained, err := oci.StoredSignatureBlobs(state, root.Digest, false)
		if err != nil {
			return err
		}
		if len(retained) != len(wantRootSignatures) {
			t.Fatalf("updated native index lost historical signatures: got %d want %d", len(retained), len(wantRootSignatures))
		}
		for i, want := range wantRootSignatures {
			if !bytes.Equal(retained[i], want) {
				t.Fatal("updated native index changed historical signature")
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, remove := range []bool{false, true} {
		t.Run("immutable-root/"+map[bool]string{false: "preserve", true: "remove"}[remove], func(t *testing.T) {
			dst := Destination{Transport: "dir", Name: filepath.Join(t.TempDir(), "output")}
			if _, err := Copy(ctx, oci.Image, root.Digest.String(), dst, Options{BuildStore: store, Push: PushOptions{RemoveSignatures: remove}}); err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(filepath.Join(dst.Name, "manifest.json"))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, raw) {
				t.Fatalf("immutable root followed mutable list update: %s", got)
			}
			paths, err := filepath.Glob(filepath.Join(dst.Name, "signature-*"))
			if err != nil {
				t.Fatal(err)
			}
			if remove {
				if len(paths) != 0 {
					t.Fatalf("historical root retained %d removed signatures", len(paths))
				}
				return
			}
			if len(paths) != len(wantRootSignatures) {
				t.Fatalf("historical root signatures = %d want %d", len(paths), len(wantRootSignatures))
			}
			for i, want := range wantRootSignatures {
				got, err := os.ReadFile(filepath.Join(dst.Name, fmt.Sprintf("signature-%d", i+1)))
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(got, want) {
					t.Fatalf("historical root encoded signature %d changed", i+1)
				}
			}
		})
	}

	t.Run("reject-native-source", func(t *testing.T) {
		policy := filepath.Join(t.TempDir(), "policy.json")
		if err := os.WriteFile(policy, []byte(`{"default":[{"type":"insecureAcceptAnything"}],"transports":{"containers-storage":{"":[{"type":"reject"}]}}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		dst := Destination{Transport: "dir", Name: filepath.Join(t.TempDir(), "output")}
		if _, err := Copy(ctx, oci.Image, "index:latest", dst, Options{BuildStore: store, SignaturePolicyPath: policy}); err == nil || !strings.Contains(strings.ToLower(err.Error()), "policy") {
			t.Fatalf("native source rejection was bypassed or failed outside policy: %v", err)
		}
	})
}

// Model the native list changing after WriteStoredIndex returns but before
// signature retention obtains its lock. A stale target must never receive
// signatures or default metadata intended for its old root.
func TestRetainIndexSignaturesRejectsChangedRoot(t *testing.T) {
	ctx := context.Background()
	store := nativeTestStore(t.TempDir())
	old, _, _ := signedIndexFixture(t, store, "before")
	var imageID string
	if err := buildah.WithStore(store, func(backend storage.Store) error {
		image, err := backend.Image("localhost/index:latest")
		if err == nil {
			imageID = image.ID
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	current, raw, selections := signedIndexFixture(t, store, "after")
	if err := buildah.WithStore(store, func(backend storage.Store) error {
		lock, err := manifests.LockerForImage(backend, imageID)
		if err != nil {
			return err
		}
		lock.Lock()
		defer lock.Unlock()
		ids := map[digest.Digest]string{}
		for _, selected := range selections {
			ids[selected.Manifest.Digest] = selected.ImageID
		}
		if _, err := imagestore.FromStore(backend).UpdateStoredIndex(ctx, imageID, current, raw, ids, "index:latest"); err != nil {
			return err
		}
		state, err := oci.CaptureStoredSignatures(backend, imageID)
		if err != nil {
			return err
		}
		signatures := [][]byte{[]byte("\x00sigstore-json\n{\"mimeType\":\"application/vnd.dev.cosign.simplesigning.v1+json\",\"payload\":\"cGF5bG9hZA==\",\"annotations\":{\"coopr.test\":\"new-root\"}}")}
		return oci.WriteStoredManifestSignatures(backend, imageID, current.Digest, true, signatures, state)
	}); err != nil {
		t.Fatal(err)
	}
	if err := buildah.WithStore(store, func(backend storage.Store) error {
		before, err := oci.CaptureStoredSignatures(backend, imageID)
		if err != nil {
			return err
		}
		err = retainIndexSignatures(backend, imageID, old.Digest)
		if err == nil || !strings.Contains(err.Error(), old.Digest.String()) || !strings.Contains(err.Error(), current.Digest.String()) {
			t.Errorf("stale root retention did not report expected and current root: %v", err)
		}
		after, err := oci.CaptureStoredSignatures(backend, imageID)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(before, after) {
			t.Fatal("rejected stale retention changed native signature metadata or blobs")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
