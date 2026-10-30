package buildah

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestBuildPlanSupervisedMaterializesDescriptorAuthFile(t *testing.T) {
	f := newLocalComponentFixture(t)
	f.write(t, "payload", "descriptor credentials stay outside the image\n")
	for _, fromEnvironment := range []bool{false, true} {
		t.Run(fmt.Sprintf("environment=%t", fromEnvironment), func(t *testing.T) {
			descriptor := descriptorRegistryAuthFile(t)
			options := SupervisedPlanOptions{
				Store: f.options.Store, ContextDir: f.contextDir,
				Isolation: "rootless", Runtime: "crun", AuthFile: descriptor,
				Output: Output{Path: filepath.Join(f.root, fmt.Sprintf("descriptor-%t", fromEnvironment))},
				Stdout: io.Discard, Stderr: io.Discard,
			}
			if fromEnvironment {
				options.AuthFile = ""
				t.Setenv("REGISTRY_AUTH_FILE", descriptor)
			}
			result, err := BuildPlanSupervised(f.ctx, testPlan(t, `from "scratch" { copy "payload" "/payload"; }`), options)
			if err != nil {
				t.Fatal(err)
			}
			if result.ManifestDigest == "" {
				t.Fatal("worker did not publish an image")
			}
			if got := localComponentLastFile(t, options.Output.Path, "payload"); got != "descriptor credentials stay outside the image\n" {
				t.Fatalf("worker image payload = %q", got)
			}
		})
	}
}

func TestStoredTransferMaterializesDescriptorAuthFile(t *testing.T) {
	f := newLocalComponentFixture(t)
	f.write(t, "payload", "native transfer\n")
	image, _ := f.build(t, "source", `from "scratch" { copy "payload" "/payload"; }`)
	for _, fromEnvironment := range []bool{false, true} {
		t.Run(fmt.Sprintf("environment=%t", fromEnvironment), func(t *testing.T) {
			descriptor := descriptorRegistryAuthFile(t)
			layout := filepath.Join(f.root, fmt.Sprintf("transfer-%t", fromEnvironment))
			options := StoredTransferOptions{
				Store: f.options.Store, ImageID: image.ImageID,
				Destination: "oci:" + layout, AuthFile: descriptor,
				SignaturePolicyPath: f.options.SystemContext.SignaturePolicyPath,
			}
			if fromEnvironment {
				options.AuthFile = ""
				t.Setenv("REGISTRY_AUTH_FILE", descriptor)
			}
			result, err := TransferStoredImageResultSupervised(f.ctx, options)
			if err != nil {
				t.Fatal(err)
			}
			if result.Manifest.Digest == "" {
				t.Fatal("worker did not copy the image")
			}
			if got := localComponentLastFile(t, layout, "payload"); got != "native transfer\n" {
				t.Fatalf("transferred image payload = %q", got)
			}
		})
	}
}

func descriptorRegistryAuthFile(t *testing.T) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	if _, err := w.Write([]byte(`{"auths":{}}`)); err != nil {
		_ = w.Close()
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("/dev/fd/%d", r.Fd())
}
