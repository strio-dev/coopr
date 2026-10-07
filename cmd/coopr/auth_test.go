package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"go.podman.io/image/v5/pkg/docker/config"
	"go.podman.io/image/v5/types"
)

func TestRegistryLoginLogoutUsesNativeCredentialFile(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		username, password, ok := r.BasicAuth()
		if !ok || username != "builder" || password != "secret" {
			w.Header().Set("WWW-Authenticate", `Basic realm="test"`)
			w.WriteHeader(http.StatusUnauthorized)
		}
	}))
	defer server.Close()
	registry := strings.TrimPrefix(server.URL, "http://")
	file := filepath.Join(t.TempDir(), "auth.json")
	var stdout, stderr bytes.Buffer
	cmd := newRootCommand()
	cmd.SetIn(strings.NewReader("secret\n"))
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"login", "--authfile", file, "--tls-verify=false", "--username", "builder", "--password-stdin", registry})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	credentials, err := config.GetCredentials(&types.SystemContext{AuthFilePath: file}, registry)
	if err != nil || credentials.Username != "builder" || credentials.Password != "secret" {
		t.Fatalf("native credential read: username=%q err=%v", credentials.Username, err)
	}
	stdout.Reset()
	if status := run([]string{"login", "--authfile", file, "--get-login", registry}, &stdout, &stderr); status != 0 || strings.TrimSpace(stdout.String()) != "builder" {
		t.Fatalf("get-login status=%d stdout=%q stderr=%q", status, &stdout, &stderr)
	}
	if strings.Contains(stderr.String(), "secret") || strings.Contains(stdout.String(), "secret") {
		t.Fatal("credentials leaked into command output")
	}
	if status := run([]string{"logout", "--authfile", file, registry}, &stdout, &stderr); status != 0 {
		t.Fatalf("logout status=%d stderr=%q", status, &stderr)
	}
	credentials, err = config.GetCredentials(&types.SystemContext{AuthFilePath: file}, registry)
	if err != nil || credentials.Username != "" {
		t.Fatalf("logout did not remove credentials: username=%q err=%v", credentials.Username, err)
	}
}

func TestRegistryLoginRejectsConflictingPasswordOptions(t *testing.T) {
	cmd := newLoginCommand()
	cmd.SetIn(strings.NewReader("secret\n"))
	cmd.SetArgs([]string{"--password", "one", "--password-stdin", "--username", "user", "registry.example.com"})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "password") {
		t.Fatalf("conflicting passwords = %v", err)
	}
}
