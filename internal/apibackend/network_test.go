package apibackend

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/strio-dev/coopr/api"
)

func TestRunNetworkNativeValidationAndKinds(t *testing.T) {
	for _, kind := range []string{"execute", "build", "component"} {
		for _, network := range []string{"", "none", "default", "host"} {
			t.Run(kind+"/"+network, func(t *testing.T) {
				req := request(t)
				req.Kind = kind
				req.Options.Network = network
				if kind != "execute" {
					file := filepath.Join(req.Options.ContextDir, "build.coopr")
					if err := os.WriteFile(file, []byte("from scratch {}"), 0600); err != nil {
						t.Fatal(err)
					}
					req.Program = nil
					req.Build = &api.Build{Files: []string{file}}
				}
				if err := validate(req); err != nil {
					t.Fatalf("native network rejected: %v", err)
				}
			})
		}
	}
	for _, kind := range []string{"publish", "component-load"} {
		req := api.Request{Kind: kind, Options: api.Options{Network: "none"}, ComponentLoad: &api.ComponentLoad{}}
		if kind == "publish" {
			req.ComponentLoad = nil
			req.Publish = &api.Publish{Artifact: "image", Source: "image", Destinations: []string{"oci:/unused"}}
		}
		if err := validate(req); err == nil {
			t.Fatalf("%s ignored RUN network", kind)
		}
	}
}

func TestInvalidRunNetworkFailsBeforeResources(t *testing.T) {
	for _, kind := range []string{"execute", "build", "component"} {
		t.Run(kind, func(t *testing.T) {
			req := request(t)
			req.Kind = kind
			req.Options.Network = filepath.Join(filepath.Dir(req.Options.ContextDir), "missing-netns")
			if kind != "execute" {
				file := filepath.Join(req.Options.ContextDir, "build.coopr")
				if err := os.WriteFile(file, []byte("from scratch {}"), 0600); err != nil {
					t.Fatal(err)
				}
				req.Program = nil
				req.Build = &api.Build{Files: []string{file}}
			}
			_, err := Execute(t.Context(), req, io.Discard, io.Discard)
			var failure *api.Error
			if !errors.As(err, &failure) || failure.Code != "invalid_request" || !strings.Contains(err.Error(), "RUN network") {
				t.Fatalf("network preflight: %v", err)
			}
			for _, path := range []string{req.Options.Store.RunRoot, req.Options.Store.GraphRoot, req.Options.Output.OCI} {
				if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("invalid network mutated %s: %v", path, err)
				}
			}
		})
	}
}
