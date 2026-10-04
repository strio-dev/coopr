package build

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"coopr/internal/oci"
	"coopr/internal/transfer"
)

func TestNormalizeBuildLogRequiresFileForSplit(t *testing.T) {
	if _, err := normalizeBuildLog("", true); err == nil {
		t.Fatal("expected --logsplit without --logfile to fail")
	}
	path, err := normalizeBuildLog("build.log", false)
	if err != nil || !filepath.IsAbs(path) {
		t.Fatalf("path=%q err=%v", path, err)
	}
}

func TestPlatformLogPathIncludesFullPlatform(t *testing.T) {
	if got := platformLogPath("build.log", "linux/arm64/v8"); got != "build.log_linux_arm64_v8" {
		t.Fatalf("path=%q", got)
	}
}

func TestQuietBuildWritersDiscardProgress(t *testing.T) {
	stdout, stderr := buildWriters(nil, nil, true, nil)
	if stdout != io.Discard || stderr != io.Discard {
		t.Fatal("quiet writers did not discard progress")
	}
}

func TestCompletedTagProgressSkipsFailedPendingAndArchives(t *testing.T) {
	destinations := []transfer.Destination{
		{Transport: "local", Name: "app:latest"},
		{Transport: "podman", Name: "localhost/app:latest"},
		{Transport: "docker", Name: "app:failed"},
		{Transport: "registry", Name: "example/app:pending"},
		{Transport: "oci-archive", Name: "image.tar"},
	}
	report := outputReport{Destinations: []destinationResult{
		{Status: "complete"}, {Status: "complete"}, {Status: "failed"}, {Status: "pending"}, {Status: "complete"},
	}}
	var output bytes.Buffer
	logCompletedTags(&output, destinations, report)
	if got, want := output.String(), "Successfully tagged app:latest\nSuccessfully tagged localhost/app:latest\n"; got != want {
		t.Fatalf("tags = %q, want %q", got, want)
	}
	logCompletedTags(io.Discard, destinations, report)
}

func TestCompletedTagProgressRespectsQuietAndLogfile(t *testing.T) {
	destinations := []transfer.Destination{{Transport: "local", Name: "app:latest"}}
	report := outputReport{Destinations: []destinationResult{{Status: "complete"}}}
	var terminal bytes.Buffer
	_, writer := buildWriters(nil, &terminal, true, nil)
	logCompletedTags(writer, destinations, report)
	if terminal.Len() != 0 {
		t.Fatalf("quiet tags leaked to terminal: %q", terminal.String())
	}
	log, err := openBuildLog(filepath.Join(t.TempDir(), "build.log"))
	if err != nil {
		t.Fatal(err)
	}
	_, writer = buildWriters(nil, &terminal, true, log)
	logCompletedTags(writer, destinations, report)
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(log.Name())
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(data), "Successfully tagged app:latest\n"; got != want {
		t.Fatalf("logfile tags = %q, want %q", got, want)
	}
	if terminal.Len() != 0 {
		t.Fatalf("logfile tags leaked to terminal: %q", terminal.String())
	}
}

func TestCompletedTagProgressDoesNotCallRegistryDigestPublicationATag(t *testing.T) {
	references := []string{
		"registry:example.com/app@sha256:" + strings.Repeat("a", 64),
		"registry:example.com/app:release",
		"registry:example.com/app",
	}
	var destinations []transfer.Destination
	report := outputReport{}
	for _, value := range references {
		destination, err := transfer.ParseDestination(value, oci.Image)
		if err != nil {
			t.Fatalf("parse actual destination %q: %v", value, err)
		}
		destinations = append(destinations, destination)
		report.Destinations = append(report.Destinations, destinationResult{Status: "complete"})
	}
	var output bytes.Buffer
	logCompletedTags(&output, destinations, report)
	want := "Successfully tagged example.com/app:release\nSuccessfully tagged example.com/app\n"
	if got := output.String(); got != want {
		t.Fatalf("registry completion = %q, want %q", got, want)
	}
}

func TestPlatformLogPathKeepsDistinctARMVariants(t *testing.T) {
	if platformLogPath("build.log", "linux/arm/v6") == platformLogPath("build.log", "linux/arm/v7") {
		t.Fatal("variant logs share a destination")
	}
}

func TestVariantLogsPassImageFinalizationAndRetainEachPlatform(t *testing.T) {
	base := filepath.Join(t.TempDir(), "build.log")
	targets, err := requestedPlatforms("", []string{"linux/arm/v6", "linux/arm/v7"})
	if err != nil {
		t.Fatal(err)
	}
	paths := make([]string, len(targets))
	for i, platform := range targets {
		paths[i] = platformLogPath(base, platform)
	}
	if _, err := finalizationArtifacts(Options{}, paths); err != nil {
		t.Fatalf("distinct platform logs rejected: %v", err)
	}
	for i, platform := range targets {
		log, err := openBuildLog(paths[i])
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(log, platform); err != nil {
			t.Fatal(err)
		}
		if err := log.Close(); err != nil {
			t.Fatal(err)
		}
	}
	for i, platform := range targets {
		data, err := os.ReadFile(paths[i])
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != platform {
			t.Fatalf("platform %s lost its log: %q", platform, data)
		}
	}
}
