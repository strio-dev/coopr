package main

import (
	"archive/tar"
	"bytes"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSBOMFlagsUseNativePresetsAndValidation(t *testing.T) {
	command := newBuildCommand()
	if scans, err := parseSBOMFlags(command); err != nil || len(scans) != 0 {
		t.Fatalf("unrequested scan=%+v %v", scans, err)
	}
	if err := command.ParseFlags([]string{"--sbom=syft-spdx", "--sbom-image-output=/sbom.json"}); err != nil {
		t.Fatal(err)
	}
	scans, err := parseSBOMFlags(command)
	if err != nil || len(scans) != 1 || scans[0].Image == "" || len(scans[0].Commands) != 2 || scans[0].ImageSBOMOutput != "/sbom.json" {
		t.Fatalf("native preset=%+v %v", scans, err)
	}
	command = newBuildCommand()
	if err := command.ParseFlags([]string{"--sbom=made-up", "--sbom-output=out.json"}); err != nil {
		t.Fatal(err)
	}
	if _, err := parseSBOMFlags(command); err == nil {
		t.Fatal("unknown preset accepted")
	}
}

func TestNativeCompressionAndConfidentialWorkloadFlags(t *testing.T) {
	command := newBuildCommand()
	disable := command.Flags().Lookup("disable-compression")
	if disable == nil || disable.DefValue != "true" {
		t.Fatalf("disable-compression flag = %#v", disable)
	}
	if command.Flags().Lookup("cw") == nil {
		t.Fatal("missing --cw flag")
	}
	definition := definitionFile(t, "from \"scratch\"\n")
	var stdout, stderr bytes.Buffer
	if code := run([]string{"build", definition, "--cw=invalid"}, &stdout, &stderr); code == 0 || !strings.Contains(stderr.String(), "parse --cw") {
		t.Fatalf("invalid --cw exit=%d stderr=%q", code, stderr.String())
	}
}

func TestBuildFlatTarStdoutLive(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live rootfs stdout export in short mode")
	}
	file := definitionFile(t, "from \"scratch\"\ncopy \"marker\" \"/marker\"\n")
	if err := os.WriteFile(filepath.Join(filepath.Dir(file), "marker"), []byte("flat output\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"-", "type=tar,dest=-"} {
		var output, diagnostics bytes.Buffer
		if code := run([]string{"build", file, "--output", value}, &output, &diagnostics); code != 0 {
			t.Fatalf("stdout export failed: %s", diagnostics.String())
		}
		reader := tar.NewReader(&output)
		found := false
		for {
			header, err := reader.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatalf("stdout is not clean tar: %v", err)
			}
			if strings.TrimPrefix(header.Name, "./") == "marker" {
				data, err := io.ReadAll(reader)
				if err != nil || string(data) != "flat output\n" {
					t.Fatalf("marker=%q %v", data, err)
				}
				found = true
			}
		}
		if !found {
			t.Fatal("tar missing marker")
		}
		lines := strings.Split(strings.TrimSpace(diagnostics.String()), "\n")
		imageID := lines[len(lines)-1]
		if decoded, err := hex.DecodeString(imageID); err != nil || len(decoded) != 32 {
			t.Fatalf("final stderr line is not a native image ID: %q", imageID)
		}
		var inspected, inspectErrors bytes.Buffer
		if code := run([]string{"image", "inspect", imageID}, &inspected, &inspectErrors); code != 0 {
			t.Fatalf("exported result is not retained in native storage: %s", &inspectErrors)
		}
	}
}
