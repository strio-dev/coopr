package build

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"coopr/internal/buildah"
	"go.podman.io/buildah/define"
)

func TestMultiPlatformHostSBOMOutputsUseLastPlatform(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH for live multi-platform SBOM output")
	}
	busybox, err := exec.LookPath("busybox")
	if err != nil {
		t.Fatal("live Buildah tests require the Nix dev shell's static busybox binary")
	}
	binary, err := os.ReadFile(busybox)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	root := dockerEngineWorkspace(t)
	t.Setenv("XDG_RUNTIME_DIR", root)
	storeDir := filepath.Join(root, "images")
	store := buildah.StoreOptions{
		GraphRoot:       filepath.Join(root, "graph"),
		RunRoot:         filepath.Join(root, "run"),
		GraphDriverName: "vfs",
	}
	if err := os.WriteFile(filepath.Join(root, "busybox"), binary, 0o700); err != nil {
		t.Fatal(err)
	}
	scannerDefinition := filepath.Join(root, "scanner.coopr")
	if err := os.WriteFile(scannerDefinition, []byte("from \"scratch\"\ncopy \"busybox\" \"/bin/busybox\" chmod=\"0755\"\ncopy \"busybox\" \"/bin/sh\" chmod=\"0755\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(ctx, Options{
		File: scannerDefinition, StoreDir: storeDir, Tag: "scanner:latest",
		Platform: "linux/" + runtime.GOARCH, BuildStore: store,
	}); err != nil {
		t.Fatalf("build scanner image: %v", err)
	}

	targetDefinition := filepath.Join(root, "target.coopr")
	if err := os.WriteFile(targetDefinition, []byte("from \"scratch\"\narg \"TARGETARCH\"\ncopy \"marker-$TARGETARCH\" \"/marker\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, arch := range []string{"amd64", "arm64"} {
		if err := os.WriteFile(filepath.Join(root, "marker-"+arch), []byte(arch+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	sbomOutput := filepath.Join(root, "sbom.json")
	purlOutput := filepath.Join(root, "purl.json")
	scan := define.SBOMScanOptions{
		Image: "scanner:latest",
		Commands: []string{
			`/bin/busybox sh -c 'arch=$(/bin/busybox cat {ROOTFS}/marker); printf "{\"bomFormat\":\"CycloneDX\",\"components\":[{\"name\":\"%s\",\"version\":\"1\",\"purl\":\"pkg:generic/%s@1\"}]}" "$arch" "$arch" > {OUTPUT}'`,
		},
		MergeStrategy: define.SBOMMergeStrategyCycloneDXByComponentNameAndVersion,
		SBOMOutput:    sbomOutput,
		PURLOutput:    purlOutput,
	}
	if _, err := Run(ctx, Options{
		File: targetDefinition, StoreDir: storeDir, Tag: "scanned:latest",
		Platforms: []string{"linux/amd64", "linux/arm64"}, Jobs: 0,
		BuildStore: store, SBOM: []define.SBOMScanOptions{scan},
	}); err != nil {
		t.Fatalf("build and scan target platforms: %v", err)
	}

	sbom, err := os.ReadFile(sbomOutput)
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Components []struct {
			Name string `json:"name"`
		} `json:"components"`
	}
	if err := json.Unmarshal(sbom, &document); err != nil {
		t.Fatalf("decode host SBOM %q: %v", sbom, err)
	}
	if len(document.Components) != 1 || document.Components[0].Name != "arm64" {
		t.Fatalf("host SBOM components=%+v, want only the last requested platform", document.Components)
	}
	purls, err := os.ReadFile(purlOutput)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(purls), "pkg:generic/arm64@1") || strings.Contains(string(purls), "pkg:generic/amd64@1") {
		t.Fatalf("host PURL output=%q, want only the last requested platform", purls)
	}
}
