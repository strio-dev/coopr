package apibackend

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/containerd/platforms"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/coopr/api"
	"github.com/strio-dev/coopr/internal/oci"
)

func TestNativeAPIBuildPlatformAndFinalFROMOverride(t *testing.T) {
	f := newNativeAPIFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	for _, test := range []struct{ name, definition, want string }{
		{"default", `from "scratch" { copy "busybox" "/data/busybox" }`, "linux/arm64"},
		{"explicit-final", `from "scratch" platform="linux/amd64" { copy "busybox" "/data/busybox" }`, "linux/amd64"},
		{"intermediate-override", `from "scratch" as="producer" platform="linux/amd64" { copy "busybox" "/data/busybox" }
from "scratch" { copy "/data/busybox" "/data/busybox" from="producer" }`, "linux/arm64"},
	} {
		t.Run(test.name, func(t *testing.T) {
			file := filepath.Join(f.context, test.name+".coopr")
			if err := os.WriteFile(file, []byte(test.definition), 0600); err != nil {
				t.Fatal(err)
			}
			request := f.request(test.name, test.name, "shared")
			request.Kind = "build"
			request.Program = nil
			request.Build = &api.Build{Files: []string{file}, Platform: "linux/aarch64"}
			var log strings.Builder
			result, err := Execute(ctx, request, io.Discard, &log)
			if err != nil {
				t.Fatalf("native default-platform build: %v\n%s", err, log.String())
			}
			if result.Platform != test.want {
				t.Fatalf("actual result platform = %q want %q", result.Platform, test.want)
			}
			raw, err := oci.ReadImageConfigLayout(ctx, result.OCI)
			if err != nil {
				t.Fatal(err)
			}
			var image v1.Image
			if err := json.Unmarshal(raw, &image); err != nil {
				t.Fatal(err)
			}
			if got := platforms.Format(platforms.Normalize(image.Platform)); got != result.Platform {
				t.Fatalf("image/config/result disagree: %s / %s", got, result.Platform)
			}
			var selected []struct {
				Platform string `json:"platform"`
			}
			if err := json.Unmarshal(result.Metadata["coopr.platforms"], &selected); err != nil {
				t.Fatal(err)
			}
			if len(selected) != 1 || selected[0].Platform != test.want {
				t.Fatalf("native variant descriptor metadata mismatch: %+v", selected)
			}
			data, err := os.ReadFile(filepath.Join(result.Filesystem, "data", "busybox"))
			if err != nil || len(data) == 0 {
				t.Fatalf("foreign filesystem data export requires no emulator: %d %v", len(data), err)
			}
		})
	}
}

func TestNativeAPIComponentReceivesRequestedPlatform(t *testing.T) {
	f := newNativeAPIFixture(t)
	file := filepath.Join(f.context, "component.coopr")
	if err := os.WriteFile(file, []byte(`extend as="payload" { env "PLATFORM" "selected" }`), 0600); err != nil {
		t.Fatal(err)
	}
	request := f.request("component-platform", "component", "shared")
	request.Kind = "component"
	request.Program = nil
	request.Build = &api.Build{Files: []string{file}, Target: "payload", Platform: "linux/arm64"}
	request.Options.Output.Filesystem = ""
	var log strings.Builder
	result, err := Execute(t.Context(), request, io.Discard, &log)
	if err != nil {
		t.Fatalf("component platform: %v\n%s", err, log.String())
	}
	if result.Platform != "linux/arm64" || result.OCI == "" || result.Reference == "" {
		t.Fatalf("component platform not preserved: %+v", result)
	}
}

func TestLiteralAPIPlatformRemainsNativeDataBuild(t *testing.T) {
	f := newNativeAPIFixture(t)
	request := f.request("literal-platform", "literal", "shared")
	request.Program.Platform = "linux/arm64"
	request.Program.States[0].Operations = []api.Operation{{Kind: "copy", Sources: []string{"busybox"}, Destination: "/data/busybox"}}
	result, err := Execute(t.Context(), request, io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if result.Platform != "linux/arm64" {
		t.Fatalf("typed native platform changed: %s", result.Platform)
	}
	raw, err := oci.ReadImageConfigLayout(t.Context(), result.OCI)
	if err != nil {
		t.Fatal(err)
	}
	var image v1.Image
	if err := json.Unmarshal(raw, &image); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprintf("%s/%s", image.OS, image.Architecture) != "linux/arm64" {
		t.Fatalf("typed final image mislabeled: %+v", image.Platform)
	}
}
