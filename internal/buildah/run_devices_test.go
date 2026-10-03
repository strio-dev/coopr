package buildah

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"tags.cncf.io/container-device-interface/pkg/cdi"
)

func TestResolveRunDeviceSpecsMatchesBuildKitSelectors(t *testing.T) {
	cache := testCDICache(t)

	got, err := resolveRunDeviceSpecs(cache, []runDeviceRequest{
		{Name: "vendor.example/device=beta", Required: true},
		{Name: "vendor.example/device"},
		{Name: "vendor.example/device=*"},
		{Name: "accelerator"},
		{Name: "missing"},
	})
	if err != nil {
		t.Fatal(err)
	}

	want := []string{
		"vendor.example/device=beta",
		"vendor.example/device=alpha",
		"other.example/device=gamma",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("resolveRunDeviceSpecs() = %#v, want %#v", got, want)
	}
}

func TestResolveRunDeviceSpecsRejectsMissingRequiredDevice(t *testing.T) {
	_, err := resolveRunDeviceSpecs(testCDICache(t), []runDeviceRequest{{
		Name:     "vendor.example/device=missing",
		Required: true,
	}})
	if err == nil || !strings.Contains(err.Error(), `required device "vendor.example/device=missing" is not registered`) {
		t.Fatalf("resolveRunDeviceSpecs() error = %v, want missing required device", err)
	}
}

func TestResolveRunDeviceSpecsPreservesRequestOrderAndDeduplicates(t *testing.T) {
	got, err := resolveRunDeviceSpecs(testCDICache(t), []runDeviceRequest{
		{Name: "other.example/device=gamma", Required: true},
		{Name: "vendor.example/device=*", Required: true},
		{Name: "vendor.example/device=beta", Required: true},
	})
	if err != nil {
		t.Fatal(err)
	}

	want := []string{
		"other.example/device=gamma",
		"vendor.example/device=alpha",
		"vendor.example/device=beta",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("resolveRunDeviceSpecs() = %#v, want %#v", got, want)
	}
}

func testCDICache(t *testing.T) *cdi.Cache {
	t.Helper()

	dir := testCDISpecDir(t)
	cache, err := cdi.NewCache(cdi.WithAutoRefresh(false), cdi.WithSpecDirs(dir))
	if err != nil {
		t.Fatal(err)
	}
	if err := cache.Refresh(); err != nil {
		t.Fatal(err)
	}
	return cache
}

func testCDISpecDir(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	specs := []struct{ name, contents string }{
		{"vendor.yaml", `cdiVersion: "0.6.0"
kind: "vendor.example/device"
devices:
  - name: beta
    annotations:
      org.mobyproject.buildkit.device.class: accelerator
      org.mobyproject.buildkit.device.autoallow: "true"
    containerEdits:
      env:
        - BETA=1
  - name: alpha
    containerEdits:
      env:
        - ALPHA=1
`},
		{"other.yaml", `cdiVersion: "0.6.0"
kind: "other.example/device"
devices:
  - name: gamma
    annotations:
      org.mobyproject.buildkit.device.class: accelerator
    containerEdits:
      env:
        - GAMMA=1
`},
	}
	for _, spec := range specs {
		if err := os.WriteFile(filepath.Join(dir, spec.name), []byte(spec.contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}
