package buildah

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"coopr/internal/buildcontext"
	"coopr/internal/definition"
	"coopr/internal/planner"
	"github.com/opencontainers/go-digest"
)

func TestResolveHTTPSourceDateEpoch(t *testing.T) {
	archive := sourceDateEpochArchive(t, map[string]int64{"old": 10, "new": 42})
	tests := []struct {
		name, lastModified string
		body               []byte
		want               *int64
	}{
		{name: "last modified", lastModified: time.Unix(99, 0).UTC().Format(http.TimeFormat), body: []byte("plain"), want: epochValue(99)},
		{name: "archive maximum", body: archive, want: epochValue(42)},
		{name: "plain source has no epoch", body: []byte("plain")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
				if test.lastModified != "" {
					response.Header().Set("Last-Modified", test.lastModified)
				}
				_, _ = response.Write(test.body)
			}))
			defer server.Close()
			got, err := resolveHTTPSourceDateEpoch(context.Background(), server.URL+"/source", "", buildCredentialSource{})
			if err != nil {
				t.Fatal(err)
			}
			if !equalSourceDateEpoch(got, test.want) {
				t.Fatalf("epoch = %v, want %v", got, test.want)
			}
		})
	}
}

func TestResolveHTTPSourceDateEpochVerifiesChecksum(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Last-Modified", time.Unix(99, 0).UTC().Format(http.TimeFormat))
		_, _ = response.Write([]byte("content"))
	}))
	defer server.Close()
	want := digest.FromString("other").String()
	if _, err := resolveHTTPSourceDateEpoch(context.Background(), server.URL, want, buildCredentialSource{}); err == nil {
		t.Fatal("checksum mismatch was accepted")
	}
}

func TestSourceDateEpochResolverNamedNonRemoteContextsResolveUnset(t *testing.T) {
	resolve := sourceDateEpochResolver(context.Background(), buildCredentialSource{})
	for _, spec := range []buildcontext.Spec{
		{Name: "local", Kind: buildcontext.Local, Path: t.TempDir()},
		{Name: "image", Kind: buildcontext.DockerImage, Reference: "example.invalid/image:latest"},
		{Name: "layout", Kind: buildcontext.OCILayout, Path: t.TempDir(), Reference: "latest"},
	} {
		got, err := resolve(planner.SourceDateEpochSource{Kind: planner.SourceDateEpochNamedContext, Name: spec.Name, Context: &spec})
		if err != nil {
			t.Fatalf("%s: %v", spec.Kind, err)
		}
		if got != nil {
			t.Fatalf("%s epoch = %d, want unset", spec.Kind, *got)
		}
	}
}

func TestPlanDefinitionInWorkerReconstructsSourceDateEpochResolver(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Last-Modified", time.Unix(123, 0).UTC().Format(http.TimeFormat))
		_, _ = response.Write([]byte("source"))
	}))
	defer server.Close()
	def, err := definition.Parse(strings.NewReader(fmt.Sprintf(`
arg "SOURCE_DATE_EPOCH" "metadata"
from "scratch" as="metadata"
add %q "/source"
from "scratch"
`, server.URL+"/source")))
	if err != nil {
		t.Fatal(err)
	}
	planning := planner.Options{Mode: planner.Build, Platform: "linux/" + runtime.GOARCH}
	plan, resolver, selected, err := planDefinitionInWorker(context.Background(), planWorkerRequest{
		Mode: "build", Definition: def, PlannerOptions: &planning, ResultPath: t.TempDir() + "/result.json",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resolver != nil || len(selected.all) != 0 {
		t.Fatalf("unneeded image resolution occurred: resolver=%v selected=%v", resolver, selected)
	}
	if plan.SourceDateEpoch == nil || *plan.SourceDateEpoch != 123 {
		t.Fatalf("epoch = %v, want 123", plan.SourceDateEpoch)
	}
}

func sourceDateEpochArchive(t *testing.T, files map[string]int64) []byte {
	t.Helper()
	var output bytes.Buffer
	writer := tar.NewWriter(&output)
	for name, epoch := range files {
		content := []byte(fmt.Sprintf("%s\n", name))
		if err := writer.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content)), ModTime: time.Unix(epoch, 0)}); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write(content); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func epochValue(value int64) *int64 { return &value }

func equalSourceDateEpoch(left, right *int64) bool {
	return left == nil && right == nil || left != nil && right != nil && *left == *right
}
