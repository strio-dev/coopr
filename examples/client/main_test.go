package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/strio-dev/coopr/api"
	"github.com/strio-dev/coopr/client"
)

func TestRequestInputIsBoundedAndSingleObject(t *testing.T) {
	for name, input := range map[string]string{
		"too large": strings.Repeat(" ", api.MaxRequestBytes+1),
		"trailing":  `{"kind":"execute"} {}`,
		"null":      `null`,
		"unknown":   `{"kind":"execute","unknown":true}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := readRequest(strings.NewReader(input)); err == nil {
				t.Fatal("invalid request input accepted")
			}
		})
	}
}

func TestPartialPublicationResultStaysOnStdout(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "api.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		encoder := json.NewEncoder(w)
		if r.Method == http.MethodGet && r.URL.Path == "/v1/info" {
			_ = encoder.Encode(api.Info{APIVersion: api.Version, Version: "consumer-test", Capabilities: []string{"publish"}})
			return
		}
		if r.Method != http.MethodPost || !strings.HasPrefix(r.URL.Path, "/v1/executions/") {
			http.NotFound(w, r)
			return
		}
		var request api.Request
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.Kind != "publish" || request.Publish == nil || request.Publish.Source != "built-image" {
			t.Errorf("wrong request received: %+v, %v", request, err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		_ = encoder.Encode(api.Event{Type: "stderr", Data: []byte("publication log\n")})
		_ = encoder.Encode(api.Event{Type: "result", Result: &api.Result{Destinations: []api.Destination{{Destination: "oci:/first", Status: "passed"}, {Destination: "oci:/second", Status: "failed", Error: "denied"}}}, Error: &api.Error{Code: "execution_failed", Message: "one destination failed"}})
	}))
	server.Listener = listener
	server.Start()
	t.Cleanup(server.Close)
	request := api.Request{Kind: "publish", Publish: &api.Publish{Artifact: "image", Source: "built-image", Destinations: []string{"oci:/first", "oci:/second"}}}
	input, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	err = run(t.Context(), []string{"-socket", socket}, bytes.NewReader(input), &stdout, &stderr)
	var execution *client.ExecutionError
	var structured *api.Error
	if !errors.As(err, &execution) || execution.ID == "" || !errors.As(err, &structured) || structured.Code != "execution_failed" {
		t.Fatalf("execution error lost: %v", err)
	}
	var result api.Result
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("stdout is not a structured result: %v\n%s", err, stdout.String())
	}
	if len(result.Destinations) != 2 || result.Destinations[0].Status != "passed" || result.Destinations[1].Status != "failed" {
		t.Fatalf("partial publication outcomes lost: %+v", result.Destinations)
	}
	if stderr.String() != "publication log\n" {
		t.Fatalf("logs were mixed with results: %q", stderr.String())
	}
}

func TestDependencyIndependence(t *testing.T) {
	command := exec.CommandContext(t.Context(), "go", "list", "-deps", "-json", ".")
	command.Env = append(os.Environ(), "CGO_ENABLED=0", "GOTOOLCHAIN=local")
	var stderr bytes.Buffer
	command.Stderr = &stderr
	data, err := command.Output()
	if err != nil {
		t.Fatalf("go list dependencies: %v\n%s", err, stderr.String())
	}
	allowed := map[string]bool{"example.com/client-consumer": false, "github.com/strio-dev/coopr/api": false, "github.com/strio-dev/coopr/client": false}
	decoder := json.NewDecoder(bytes.NewReader(data))
	for {
		var dependency struct {
			ImportPath string
			Standard   bool
			CgoFiles   []string
		}
		if err := decoder.Decode(&dependency); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		if len(dependency.CgoFiles) != 0 {
			t.Errorf("dependency requires CGO: %s (%v)", dependency.ImportPath, dependency.CgoFiles)
		}
		if !dependency.Standard {
			if _, ok := allowed[dependency.ImportPath]; !ok {
				t.Errorf("unexpected nonstandard dependency: %s", dependency.ImportPath)
			} else {
				allowed[dependency.ImportPath] = true
			}
		}
	}
	for path, found := range allowed {
		if !found {
			t.Errorf("expected dependency missing: %s", path)
		}
	}
}
