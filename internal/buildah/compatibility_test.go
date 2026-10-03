package buildah

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"coopr/internal/planner"
	"golang.org/x/sys/unix"
)

func TestCheckCompatibilityArchitectureDoesNotNeedRootfs(t *testing.T) {
	stages := []planner.Stage{{ID: "base", Requirements: map[string][]string{"architecture": {runtime.GOARCH}}}}
	if err := CheckCompatibility("", nil, "linux/"+runtime.GOARCH, stages); err != nil {
		t.Fatal(err)
	}
	stages[0].Requirements["architecture"] = []string{differentArchitecture(runtime.GOARCH)}
	if err := CheckCompatibility("", nil, "linux/"+runtime.GOARCH, stages); err == nil || !strings.Contains(err.Error(), "requires one of architectures") {
		t.Fatalf("expected architecture mismatch, got %v", err)
	}
}

func TestCheckCompatibilityDistroUsesEtcThenUsrLibFallback(t *testing.T) {
	rootfs := t.TempDir()
	writeCompatibilityFile(t, rootfs, "usr/lib/os-release", "ID=alpine\nVERSION_ID=3.20\n", 0o644)
	stages := []planner.Stage{{ID: "caller", Requirements: map[string][]string{"distro": {"alpine"}, "distro-version": {"3.20"}}}}
	if err := CheckCompatibility(rootfs, nil, "linux/"+runtime.GOARCH, stages); err != nil {
		t.Fatal(err)
	}

	writeCompatibilityFile(t, rootfs, "etc/os-release", "ID=debian\nVERSION_ID=12\n", 0o644)
	err := CheckCompatibility(rootfs, nil, "linux/"+runtime.GOARCH, stages)
	if err == nil || !strings.Contains(err.Error(), `requires one of distros ["alpine"], caller has "debian"`) {
		t.Fatalf("expected /etc/os-release to take precedence, got %v", err)
	}
}

func TestCheckCompatibilityAllowsAnyValueWithinEachRequirement(t *testing.T) {
	rootfs := t.TempDir()
	writeCompatibilityFile(t, rootfs, "etc/os-release", "ID=fedora\nVERSION_ID=42\n", 0o644)
	writeCompatibilityFile(t, rootfs, "usr/bin/dnf", "#!/bin/sh\n", 0o755)
	stages := []planner.Stage{{ID: "caller", Requirements: map[string][]string{
		"distro":          {"rhel", "fedora"},
		"distro-version":  {"41", "42"},
		"package-manager": {"yum", "dnf"},
		"architecture":    {differentArchitecture(runtime.GOARCH), runtime.GOARCH},
	}}}
	if err := CheckCompatibility(rootfs, nil, "linux/"+runtime.GOARCH, stages); err != nil {
		t.Fatal(err)
	}
	stages[0].Requirements["distro-version"] = []string{"40", "41"}
	if err := CheckCompatibility(rootfs, nil, "linux/"+runtime.GOARCH, stages); err == nil || !strings.Contains(err.Error(), "requires one of distro-versions") {
		t.Fatalf("expected version mismatch, got %v", err)
	}
}

func TestCheckCompatibilityPackageManagerUsesImagePath(t *testing.T) {
	rootfs := t.TempDir()
	writeCompatibilityFile(t, rootfs, "opt/tools/pkg", "#!/bin/sh\n", 0o755)
	config := json.RawMessage(`{"config":{"Env":["PATH=/ignored","PATH=relative:/opt/tools"]}}`)
	stages := []planner.Stage{{ID: "caller", Requirements: map[string][]string{"package-manager": {"pkg"}}}}
	if err := CheckCompatibility(rootfs, config, "linux/"+runtime.GOARCH, stages); err != nil {
		t.Fatal(err)
	}

	if err := os.Chmod(filepath.Join(rootfs, "opt/tools/pkg"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := CheckCompatibility(rootfs, config, "linux/"+runtime.GOARCH, stages)
	if err == nil || !strings.Contains(err.Error(), "no executable package-manager") {
		t.Fatalf("expected non-executable manager rejection, got %v", err)
	}
}

func TestCheckCompatibilityPackageManagerAbsoluteAlternativeDoesNotNeedImagePath(t *testing.T) {
	rootfs := t.TempDir()
	writeCompatibilityFile(t, rootfs, "usr/bin/pkg", "#!/bin/sh\n", 0o755)
	stages := []planner.Stage{{ID: "caller", Requirements: map[string][]string{
		"package-manager": {"relative-needs-path", "/usr/bin/pkg"},
	}}}
	if err := CheckCompatibility(rootfs, json.RawMessage(`{`), "linux/"+runtime.GOARCH, stages); err != nil {
		t.Fatal(err)
	}
}

func TestCheckCompatibilityPackageManagerAbsoluteSymlinkStaysInRoot(t *testing.T) {
	rootfs := t.TempDir()
	writeCompatibilityFile(t, rootfs, "usr/libexec/pkg", "#!/bin/sh\n", 0o755)
	if err := os.MkdirAll(filepath.Join(rootfs, "usr/bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/usr/libexec/pkg", filepath.Join(rootfs, "usr/bin/pkg")); err != nil {
		t.Fatal(err)
	}
	stages := []planner.Stage{{ID: "caller", Requirements: map[string][]string{"package-manager": {"/usr/bin/pkg"}}}}
	if err := CheckCompatibility(rootfs, nil, "linux/"+runtime.GOARCH, stages); err != nil {
		t.Fatal(err)
	}
}

func TestCheckCompatibilityPackageManagerCannotEscapeRoot(t *testing.T) {
	rootfs := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside-pkg")
	if err := os.WriteFile(outside, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(rootfs, "usr/bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(rootfs, "usr/bin/pkg")); err != nil {
		t.Fatal(err)
	}
	stages := []planner.Stage{{ID: "caller", Requirements: map[string][]string{"package-manager": {"/usr/bin/pkg"}}}}
	err := CheckCompatibility(rootfs, nil, "linux/"+runtime.GOARCH, stages)
	if err == nil || !strings.Contains(err.Error(), "no executable package-manager") {
		t.Fatalf("expected escaped symlink rejection, got %v", err)
	}
}

func TestCheckCompatibilityDoesNotBlockOnSpecialFiles(t *testing.T) {
	rootfs := t.TempDir()
	manager := filepath.Join(rootfs, "usr/bin/pkg")
	if err := os.MkdirAll(filepath.Dir(manager), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(manager, 0o755); err != nil {
		t.Fatal(err)
	}
	stages := []planner.Stage{{ID: "caller", Requirements: map[string][]string{"package-manager": {"/usr/bin/pkg"}}}}
	done := make(chan error, 1)
	go func() { done <- CheckCompatibility(rootfs, nil, "linux/"+runtime.GOARCH, stages) }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "no executable package-manager") {
			t.Fatalf("expected FIFO rejection, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("compatibility check blocked while opening FIFO")
	}
}

func TestCheckCompatibilityValidatesEveryExtendRoot(t *testing.T) {
	rootfs := t.TempDir()
	writeCompatibilityFile(t, rootfs, "etc/os-release", "ID=alpine\nVERSION_ID=3.20\n", 0o644)
	stages := []planner.Stage{
		{ID: "first", Requirements: map[string][]string{"distro": {"alpine"}}},
		{ID: "second", Requirements: map[string][]string{"distro": {"alpine"}, "distro-version": {"3.19"}}},
	}
	err := CheckCompatibility(rootfs, nil, "linux/"+runtime.GOARCH, stages)
	if err == nil || !strings.Contains(err.Error(), "extend stage second requires one of distro-versions") {
		t.Fatalf("expected second root rejection, got %v", err)
	}
}

func TestCheckCompatibilityRejectsMalformedInputs(t *testing.T) {
	rootfs := t.TempDir()
	tests := []struct {
		name   string
		config json.RawMessage
		stage  planner.Stage
		want   string
	}{
		{"version without distro", nil, planner.Stage{ID: "x", Requirements: map[string][]string{"distro-version": {"1"}}}, "requires distro"},
		{"unknown requirement", nil, planner.Stage{ID: "x", Requirements: map[string][]string{"kernel": {"6"}}}, "unknown compatibility requirement"},
		{"invalid manager", nil, planner.Stage{ID: "x", Requirements: map[string][]string{"package-manager": {"../pkg"}}}, "invalid package-manager"},
		{"malformed config", json.RawMessage(`{`), planner.Stage{ID: "x", Requirements: map[string][]string{"package-manager": {"pkg"}}}, "decode caller PATH"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := CheckCompatibility(rootfs, test.config, "linux/"+runtime.GOARCH, []planner.Stage{test.stage})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("expected %q, got %v", test.want, err)
			}
		})
	}
}

func TestCheckCompatibilityRejectsMalformedOSRelease(t *testing.T) {
	rootfs := t.TempDir()
	writeCompatibilityFile(t, rootfs, "etc/os-release", "ID=\"unterminated\n", 0o644)
	stages := []planner.Stage{{ID: "caller", Requirements: map[string][]string{"distro": {"alpine"}}}}
	err := CheckCompatibility(rootfs, nil, "linux/"+runtime.GOARCH, stages)
	if err == nil || !strings.Contains(err.Error(), "malformed os-release ID") {
		t.Fatalf("expected malformed os-release rejection, got %v", err)
	}
}

func writeCompatibilityFile(t *testing.T, root, name, contents string, mode os.FileMode) {
	t.Helper()
	filename := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, []byte(contents), mode); err != nil {
		t.Fatal(err)
	}
}

func differentArchitecture(architecture string) string {
	if architecture == "amd64" {
		return "arm64"
	}
	return "amd64"
}
