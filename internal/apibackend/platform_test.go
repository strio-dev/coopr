package apibackend

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/strio-dev/coopr/api"
	"golang.org/x/sys/unix"
)

func TestNormalizeBuildPlatform(t *testing.T) {
	for _, test := range []struct{ value, want string }{{"", ""}, {"linux/x86_64", "linux/amd64"}, {"linux/aarch64", "linux/arm64"}, {"linux/arm/v7", "linux/arm/v7"}} {
		got, err := normalizeBuildPlatform(test.value)
		if err != nil || got != test.want {
			t.Fatalf("normalize %q = %q %v", test.value, got, err)
		}
	}
	for _, value := range []string{" ", "darwin/arm64", "windows/amd64", "linux/amd64,linux/arm64", "linux/arm/extra/invalid"} {
		if _, err := normalizeBuildPlatform(value); err == nil {
			t.Fatalf("invalid platform %q admitted", value)
		}
	}
}

func TestBuildPlatformValidationPrecedesStoreMutation(t *testing.T) {
	for _, kind := range []string{"build", "component"} {
		r := request(t)
		r.Kind = kind
		r.Program = nil
		file := filepath.Join(r.Options.ContextDir, "image.coopr")
		if err := os.WriteFile(file, []byte(`from "scratch" {}`), 0600); err != nil {
			t.Fatal(err)
		}
		r.Build = &api.Build{Files: []string{file}, Platform: "windows/amd64"}
		_, err := Execute(t.Context(), r, nil, nil)
		var apiErr *api.Error
		if !errors.As(err, &apiErr) || apiErr.Code != "invalid_request" || !strings.Contains(err.Error(), "platform") {
			t.Fatalf("%s error=%v", kind, err)
		}
		if _, err := os.Stat(r.Options.Store.GraphRoot); !os.IsNotExist(err) {
			t.Fatalf("invalid platform mutated store: %v", err)
		}
	}
}

func TestNativeResultReportsActualPlatform(t *testing.T) {
	result := api.Result{Platform: "linux/amd64"}
	if err := applyNativeResultMetadata(&result, []byte(`{"coopr.platforms":[{"platform":"linux/aarch64"}]}`)); err != nil {
		t.Fatal(err)
	}
	if result.Platform != "linux/arm64" {
		t.Fatalf("reported host/default instead of actual: %+v", result)
	}
	for _, raw := range []string{`{"coopr.platforms":[{"platform":"windows/amd64"}]}`} {
		if err := applyNativeResultMetadata(&api.Result{}, []byte(raw)); err == nil {
			t.Fatalf("bad actual platform admitted: %s", raw)
		}
	}
	for _, raw := range []string{`{"coopr.platforms":[]}`, `{"coopr.platforms":[{"platform":"linux/amd64"},{"platform":"linux/arm64"}]}`} {
		result := api.Result{Platform: "linux/amd64"}
		if err := applyNativeResultMetadata(&result, []byte(raw)); err != nil || result.Platform != "" {
			t.Fatalf("ambiguous platform reported host: %+v %v", result, err)
		}
	}
}

func TestPlatformExecutionErrorPreservesCauseAndClassification(t *testing.T) {
	wrapped := platformExecutionError(unix.ENOEXEC, "linux/arm64")
	if !errors.Is(wrapped, unix.ENOEXEC) || !strings.Contains(wrapped.Error(), "binfmt_misc") || !strings.Contains(wrapped.Error(), "linux/arm64") {
		t.Fatalf("missing actionable native error: %v", wrapped)
	}
	canceled := platformExecutionError(context.Canceled, "linux/arm64")
	var apiErr *api.Error
	if !errors.As(executionError(canceled), &apiErr) || apiErr.Code != "canceled" {
		t.Fatal("cancellation classification changed")
	}
	failure := errors.New("native runtime failure")
	if platformExecutionError(failure, "linux/arm64") != failure {
		t.Fatal("unrelated native error changed")
	}
}
