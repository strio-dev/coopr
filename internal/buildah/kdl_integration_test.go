package buildah

import (
	"fmt"
	"io"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestLocalComponentNativeOctalPermissionsBuildAndCache(t *testing.T) {
	f := newLocalComponentFixture(t)
	base := newLiveBusyBoxStorage(t, f.ctx, f.root, f.options.Store)
	f.write(t, "payload", "native permissions\n")
	f.write(t, "token", "mounted secret\n")
	f.write(t, "components/native.coopr", `package as="assets"
copy "payload" "/payload" chmod=0o640
extend
copy "/payload" "/payload" from="assets"
copy "/payload" "/tool" from="assets" chmod=0o755
run "test \"$(/bin/busybox stat -c %a /payload)\" = 640 && test \"$(/bin/busybox stat -c %a /tool)\" = 755 && test \"$(/bin/busybox stat -c %a /run/token)\" = 400 && cat /tool /run/token >/proof && od -An -N16 -tx1 /dev/urandom >>/proof" {
    mount "secret" id="token" target="/run/token" required=#true mode=0o400
}
`)
	plan := testPlan(t, fmt.Sprintf("from %q\ncomponent \"./components/native.coopr\"\n", base.reference))
	var coldManifest v1.Manifest
	var coldImage v1.Image
	var coldProof string
	for _, attempt := range []string{"cold", "warm"} {
		t.Run(attempt, func(t *testing.T) {
			layout := filepath.Join(f.root, attempt)
			var logs strings.Builder
			_, err := BuildPlanSupervised(f.ctx, plan, SupervisedPlanOptions{
				Store: f.options.Store, ContextDir: f.contextDir, Isolation: "rootless", Runtime: "crun",
				Output: Output{Path: layout}, ComponentStoreDir: filepath.Join(f.root, "components"),
				CacheLocalDir: f.options.CacheLocalDir, SignaturePolicyPath: f.options.SystemContext.SignaturePolicyPath,
				Secrets: []string{"id=token,src=" + filepath.Join(f.contextDir, "token")},
				Stdout:  io.Discard, Stderr: &logs,
			})
			if err != nil {
				t.Fatal(err)
			}
			manifest, image := readPlanImage(t, layout)
			if len(manifest.Layers) < 3 {
				t.Fatalf("expected separate COPY/COPY/RUN layers, got %d", len(manifest.Layers))
			}
			for index, file := range []struct {
				name string
				mode int64
			}{{"payload", 0o640}, {"tool", 0o755}} {
				layer := manifest.Layers[len(manifest.Layers)-3+index]
				blob := filepath.Join(layout, "blobs", "sha256", layer.Digest.Encoded())
				if got := readLayerHeader(t, blob, file.name).Mode & 0o777; got != file.mode {
					t.Errorf("%s mode = %#o, want %#o", file.name, got, file.mode)
				}
			}
			proof := localComponentLastFile(t, layout, "proof")
			if !strings.HasPrefix(proof, "native permissions\nmounted secret\n") {
				t.Fatalf("native mode/secret proof = %q", proof)
			}
			if attempt == "cold" {
				coldManifest, coldImage, coldProof = manifest, image, proof
				return
			}
			if !strings.Contains(logs.String(), "--> Using component cache ") {
				t.Fatalf("warm component did not restore cache:\n%s", logs.String())
			}
			if proof != coldProof {
				t.Fatalf("warm component reran random proof: cold=%q warm=%q", coldProof, proof)
			}
			if !reflect.DeepEqual(manifest, coldManifest) || !reflect.DeepEqual(image, coldImage) {
				t.Fatal("warm component changed the cached image chain or configuration")
			}
		})
	}
}
