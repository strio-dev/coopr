package buildah

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/strio-dev/coopr/internal/oci"
	"go.podman.io/image/v5/types"
)

func TestPortablePackageCacheSeparatesVolumeCompatibility(t *testing.T) {
	if testing.Short() {
		t.Skip("requires live rootless Buildah")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	root := t.TempDir()
	base, _ := newLiveBusyBoxRegistry(t, ctx)
	resolver, err := oci.NewResolver(oci.Options{TLSVerify: new(false), ComponentStoreDir: filepath.Join(root, "components")})
	if err != nil {
		t.Fatal(err)
	}
	policy := writeComponentTestPolicy(t, root)
	plan := testPublicationPlan(t, `from "`+base+`" as="producer" {
 volume "/data"
 run "/bin/busybox mkdir -p /data; echo volume-write >/data/proof"
}
package as="bundle" { copy "/data" "/data" from="producer" }
extend { copy "/data" "/data" from="bundle" }
`)
	for _, noLayers := range []bool{false, true} {
		t.Run(fmt.Sprintf("no-layers=%v", noLayers), func(t *testing.T) {
			cacheDir := filepath.Join(root, fmt.Sprintf("cache-%v", noLayers))
			build := func(name string, compat, noCache bool, wantExecuted bool) bool {
				t.Helper()
				storeRoot := filepath.Join(root, fmt.Sprintf("%v-%s", noLayers, name))
				store := StoreOptions{RunRoot: filepath.Join(storeRoot, "run"), GraphRoot: filepath.Join(storeRoot, "graph"), GraphDriverName: "vfs"}
				path := filepath.Join(storeRoot, "bundle.tar")
				_, err := PublishPlan(ctx, plan, PublicationOptions{PlanOptions: PlanOptions{
					Store: store, ContextDir: root, Isolation: "rootless", Runtime: "crun", Resolver: resolver,
					CacheLocalDir: cacheDir, NoCache: noCache, Lifecycle: LifecycleControls{CompatVolumes: compat, NoLayers: noLayers},
					SystemContext: &types.SystemContext{SignaturePolicyPath: policy, BigFilesTemporaryDir: root},
				}, PackagePaths: map[string]string{"bundle": path}})
				if err != nil {
					t.Fatal(err)
				}
				lease, err := acquireStore(store)
				if err != nil {
					t.Fatal(err)
				}
				images, imageErr := lease.store.Images()
				closeErr := lease.Close()
				if err := errors.Join(imageErr, closeErr); err != nil {
					t.Fatal(err)
				}
				if (len(images) > 0) != wantExecuted {
					t.Errorf("%s executed=%v, want %v", name, len(images) > 0, wantExecuted)
				}
				file, err := os.Open(path)
				if err != nil {
					t.Fatal(err)
				}
				defer func() {
					if err := file.Close(); err != nil {
						t.Errorf("close package archive: %v", err)
					}
				}()
				archive := tar.NewReader(file)
				for {
					entry, err := archive.Next()
					if err == io.EOF {
						return false
					}
					if err != nil {
						t.Fatal(err)
					}
					if strings.TrimPrefix(entry.Name, "./") == "data/proof" {
						data, err := io.ReadAll(archive)
						if err != nil {
							t.Fatal(err)
						}
						if string(data) != "volume-write\n" {
							t.Fatalf("unexpected proof %q", data)
						}
						return true
					}
				}
			}
			if !build("cold-preserve", false, false, true) {
				t.Fatal("non-compatible cold build lost volume write")
			}
			if !build("warm-preserve", false, false, noLayers) {
				t.Fatal("non-compatible portable hit lost volume write")
			}
			if build("changed-discard", true, false, true) {
				t.Fatal("compatible build replayed cached non-compatible volume write")
			}
			if build("uncached-discard", true, true, true) {
				t.Fatal("uncached compatible build retained volume write")
			}
			if build("warm-discard", true, false, noLayers) {
				t.Fatal("compatible portable hit retained volume write")
			}
		})
	}
}
