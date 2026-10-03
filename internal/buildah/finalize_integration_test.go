package buildah

import (
	"archive/tar"
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

	"coopr/internal/definition"
	"coopr/internal/planner"
	"go.podman.io/buildah/define"
)

func TestOutputFinalizationRetainsBaseAndCachesInstructions(t *testing.T) {
	requireLiveInstructionCache(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	root := t.TempDir()
	store := cacheTestStore(root)
	base := newLiveBusyBoxStorage(t, ctx, root, store)
	policy := writeComponentTestPolicy(t, root)
	plan := testPlan(t, fmt.Sprintf(`from %q as="base"
run "/bin/busybox echo obsolete >/obsolete" network="none"
from "base"
run "/bin/busybox rm /obsolete; /bin/busybox echo first >/first; /bin/busybox ln -s /first /link" network="none"
run "/bin/busybox echo second >/second" network="none"
label purpose="finalization"
`, base.reference))
	var baseLayers int
	var baseDigests []string
	for i, mode := range []string{"normal", "squash", "all", "warm-squash"} {
		layout := filepath.Join(root, mode)
		tarPath := filepath.Join(root, mode+".tar")
		output := Output{Path: layout, Squash: mode == "squash" || mode == "warm-squash", SquashAll: mode == "all", Filesystem: FilesystemOutput{Type: "tar", Path: tarPath}}
		_, err := BuildPlanSupervised(ctx, plan, SupervisedPlanOptions{
			Store: store, ContextDir: root, Isolation: "rootless", Runtime: "crun", Output: output,
			ImageStoreDir: base.imageStoreDir, SignaturePolicyPath: policy, Stdout: io.Discard, Stderr: io.Discard,
		})
		if err != nil {
			t.Fatal(err)
		}
		manifest, image := readPlanImage(t, layout)
		if i == 0 {
			baseLayers = len(manifest.Layers) - 2
			for _, layer := range manifest.Layers[:baseLayers] {
				baseDigests = append(baseDigests, layer.Digest.String())
			}
		}
		want := baseLayers + 1
		if mode == "normal" {
			want = baseLayers + 2
		}
		if mode == "all" {
			want = 1
		}
		if len(manifest.Layers) != want {
			t.Fatalf("%s layers=%d want %d", mode, len(manifest.Layers), want)
		}
		if output.Squash {
			foundRun, foundSquash := false, false
			for _, entry := range image.History {
				foundRun = foundRun || strings.Contains(entry.CreatedBy, "RUN")
				foundSquash = foundSquash || entry.CreatedBy == "coopr squash"
			}
			if !foundRun || !foundSquash {
				t.Fatalf("%s lost instruction/squash history: %+v", mode, image.History)
			}
			for index, layer := range manifest.Layers[:baseLayers] {
				if layer.Digest.String() != baseDigests[index] {
					t.Fatalf("%s changed base layer %d", mode, index)
				}
			}
		}
		if image.Config.Labels["purpose"] != "finalization" {
			t.Fatalf("%s lost config: %+v", mode, image.Config)
		}
		files, links := readFlatTar(t, tarPath)
		if files["first"] != "first\n" || files["second"] != "second\n" || links["link"] != "/first" || len(files["bin/busybox"]) == 0 {
			t.Fatalf("%s filesystem missing entries; first=%q second=%q link=%q", mode, files["first"], files["second"], links["link"])
		}
		if _, exists := files["obsolete"]; exists {
			t.Fatalf("%s exported a deleted base file", mode)
		}
		if count := instructionCacheRecordCount(t, store); count != 3 {
			t.Fatalf("%s instruction cache records=%d want 3", mode, count)
		}
	}
}

func TestConfidentialWorkloadPreservesConvertedImageConfig(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH=1 for native confidential-workload conversion")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	root := t.TempDir()
	store := StoreOptions{RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs"}
	base := newLiveBusyBoxStorage(t, ctx, root, store)
	policy := writeComponentTestPolicy(t, root)
	plan := testPlan(t, fmt.Sprintf(`from %q
user "1000"
workdir "/tmp"
entrypoint { exec "/bin/busybox" }
cmd { exec "echo" "hello" }
`, base.reference))
	layout := filepath.Join(root, "layout")
	_, err := BuildPlanSupervised(ctx, plan, SupervisedPlanOptions{
		Store: store, ContextDir: root, Isolation: "rootless", Runtime: "crun", ImageStoreDir: base.imageStoreDir,
		SignaturePolicyPath: policy, Stdout: io.Discard, Stderr: io.Discard,
		Output: Output{Path: layout, ConfidentialWorkload: define.ConfidentialWorkloadOptions{
			Convert: true, TeeType: define.SNP, DiskEncryptionPassphrase: "coopr-test-passphrase", Slop: "16m", TempDir: root,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	manifest, image := readPlanImage(t, layout)
	if len(manifest.Layers) != 1 {
		t.Fatalf("confidential workload layers=%d, want one encrypted disk layer", len(manifest.Layers))
	}
	if !slices.Equal(image.Config.Entrypoint, []string{"/entrypoint"}) || len(image.Config.Cmd) != 0 || image.Config.User != "" || image.Config.WorkingDir != "" {
		t.Fatalf("confidential workload config was overwritten: entrypoint=%v cmd=%v user=%q workdir=%q", image.Config.Entrypoint, image.Config.Cmd, image.Config.User, image.Config.WorkingDir)
	}
}

func TestOutputFinalizationScratchLocalAndUnchangedBase(t *testing.T) {
	requireLiveInstructionCache(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "marker"), []byte("rootfs export\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	for _, squash := range []bool{false, true} {
		output := filepath.Join(root, fmt.Sprintf("local-%t", squash))
		_, err := BuildPlan(ctx, testPlan(t, "from \"scratch\"\ncopy \"marker\" \"/marker\"\n"), PlanOptions{
			Store: cacheTestStore(root), ContextDir: root, Isolation: "rootless", Output: Output{Path: filepath.Join(root, fmt.Sprintf("layout-%t", squash)), Squash: squash, Filesystem: FilesystemOutput{Type: "local", Path: output}},
		})
		if err != nil {
			t.Fatal(err)
		}
		manifest, image := readPlanImage(t, filepath.Join(root, fmt.Sprintf("layout-%t", squash)))
		if squash && len(manifest.Layers) != 1 {
			t.Fatalf("scratch squash layers=%d want 1", len(manifest.Layers))
		}
		if squash {
			found := false
			for _, entry := range image.History {
				found = found || strings.HasPrefix(entry.CreatedBy, "COPY")
			}
			if !found {
				t.Fatalf("lost scratch COPY history: %+v", image.History)
			}
		}
		data, err := os.ReadFile(filepath.Join(output, "marker"))
		if err != nil || string(data) != "rootfs export\n" {
			t.Fatalf("local export=%q %v", data, err)
		}
	}
}

func TestOutputCompressionControl(t *testing.T) {
	requireLiveInstructionCache(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "marker"), []byte("compression\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name               string
		disableCompression bool
		wantLayerMediaType string
	}{
		{name: "uncompressed", disableCompression: true, wantLayerMediaType: "application/vnd.oci.image.layer.v1.tar"},
		{name: "gzip", disableCompression: false, wantLayerMediaType: "application/vnd.oci.image.layer.v1.tar+gzip"},
	} {
		t.Run(test.name, func(t *testing.T) {
			layout := filepath.Join(root, test.name)
			_, err := BuildPlan(ctx, testPlan(t, "from \"scratch\"\ncopy \"marker\" \"/marker\"\n"), PlanOptions{
				Store: cacheTestStore(filepath.Join(root, test.name+"-store")), ContextDir: root, Isolation: "rootless",
				Output: Output{Path: layout, DisableCompression: test.disableCompression},
			})
			if err != nil {
				t.Fatal(err)
			}
			manifest, _ := readPlanImage(t, layout)
			if len(manifest.Layers) != 1 || manifest.Layers[0].MediaType != test.wantLayerMediaType {
				t.Fatalf("layers = %#v, want media type %s", manifest.Layers, test.wantLayerMediaType)
			}
		})
	}
}

func TestSBOMScannerImageUsesFilteredContextAndEmbedsOutput(t *testing.T) {
	requireLiveInstructionCache(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	root := t.TempDir()
	store := cacheTestStore(root)
	base := newLiveBusyBoxStorage(t, ctx, root, store)
	policy := writeComponentTestPolicy(t, root)
	if err := os.WriteFile(filepath.Join(root, "marker"), []byte("scan me\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "ignored"), []byte("hidden"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".dockerignore"), []byte("ignored\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	def, err := definition.Parse(strings.NewReader("from \"scratch\"\ncopy \"marker\" \"/marker\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	for attempt, mode := range []string{"normal", "squash", "warm-squash", "foreign-squash", "all", "host-only"} {
		platform := "linux/" + runtime.GOARCH
		if mode == "foreign-squash" {
			platform = "linux/arm64"
			if runtime.GOARCH == "arm64" {
				platform = "linux/amd64"
			}
		}
		localSBOM := filepath.Join(root, fmt.Sprintf("sbom-%d.json", attempt))
		flat := filepath.Join(root, fmt.Sprintf("sbom-%d.tar", attempt))
		localPURL := filepath.Join(root, fmt.Sprintf("purl-%d.json", attempt))
		scan := define.SBOMScanOptions{Image: base.reference, Commands: []string{
			`/bin/busybox sh -c 'test -f {ROOTFS}/marker && printf "{\"bomFormat\":\"CycloneDX\",\"components\":[{\"name\":\"demo\",\"version\":\"1\",\"purl\":\"pkg:generic/demo@1\"}]}" > {OUTPUT}'`,
			`/bin/busybox sh -c 'test -f {CONTEXT}/marker && test ! -e {CONTEXT}/ignored && test ! -e {CONTEXT}/graph && printf "{\"bomFormat\":\"CycloneDX\",\"components\":[{\"name\":\"demo\",\"version\":\"1\",\"purl\":\"pkg:generic/demo@1\"}]}" > {OUTPUT}'`,
		}, MergeStrategy: define.SBOMMergeStrategyCycloneDXByComponentNameAndVersion, SBOMOutput: localSBOM, ImageSBOMOutput: "/sbom.json", PURLOutput: localPURL, ImagePURLOutput: "/purl.json"}
		if mode == "host-only" {
			scan.ImageSBOMOutput = ""
			scan.ImagePURLOutput = ""
		}
		_, err := BuildDefinitionSupervised(ctx, def, planner.Options{Mode: planner.Build, Platform: platform}, SupervisedPlanOptions{
			Store: store, ContextDir: root, ContextArtifacts: []string{store.GraphRoot, store.RunRoot, base.imageStoreDir, localSBOM, localPURL, flat}, Isolation: "rootless", Runtime: "crun", ImageStoreDir: base.imageStoreDir, SignaturePolicyPath: policy,
			Output: Output{Path: filepath.Join(root, fmt.Sprintf("sbom-layout-%d", attempt)), SBOM: []define.SBOMScanOptions{scan}, Squash: strings.Contains(mode, "squash"), SquashAll: mode == "all", Filesystem: FilesystemOutput{Type: "tar", Path: flat}}, Stdout: io.Discard, Stderr: io.Discard,
		})
		if err != nil {
			t.Fatal(err)
		}
		manifest, image := readPlanImage(t, filepath.Join(root, fmt.Sprintf("sbom-layout-%d", attempt)))
		wantLayers := 1
		if mode == "normal" {
			wantLayers = 2
		}
		if len(manifest.Layers) != wantLayers {
			t.Fatalf("%s SBOM layers=%d want %d", mode, len(manifest.Layers), wantLayers)
		}
		if image.Architecture != strings.TrimPrefix(platform, "linux/") {
			t.Fatalf("scanner changed target platform: %s want %s", image.Architecture, platform)
		}
		data, err := os.ReadFile(localSBOM)
		if err != nil || !strings.Contains(string(data), "CycloneDX") {
			t.Fatalf("host SBOM=%q %v", data, err)
		}
		files, _ := readFlatTar(t, flat)
		if scan.ImageSBOMOutput != "" && files["sbom.json"] != string(data) {
			t.Fatalf("embedded SBOM=%q, host=%q", files["sbom.json"], data)
		}
		purl, err := os.ReadFile(localPURL)
		if err != nil || !strings.Contains(string(purl), "pkg:generic/demo@1") {
			t.Fatalf("PURL=%q %v", purl, err)
		}
		if scan.ImagePURLOutput != "" && files["purl.json"] != string(purl) {
			t.Fatalf("embedded PURL differs: %q", files["purl.json"])
		}
		lease, err := acquireStore(store)
		if err != nil {
			t.Fatal(err)
		}
		images, err := lease.store.Images()
		if err != nil {
			t.Fatal(err)
		}
		for _, stored := range images {
			raw, err := packageImageConfig(ctx, lease.store, stored.ID, nil)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(raw), "coopr temporary SBOM scan") {
				t.Error("temporary scanner image was retained")
			}
		}
		if err := lease.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func readFlatTar(t *testing.T, path string) (map[string]string, map[string]string) {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	reader := tar.NewReader(file)
	files, links := map[string]string{}, map[string]string{}
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		name := strings.TrimPrefix(header.Name, "./")
		if header.Typeflag == tar.TypeSymlink {
			links[name] = header.Linkname
		}
		if header.Typeflag == tar.TypeReg {
			data, err := io.ReadAll(reader)
			if err != nil {
				t.Fatal(err)
			}
			files[name] = string(data)
		}
	}
	return files, links
}
