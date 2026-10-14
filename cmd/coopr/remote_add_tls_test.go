package main

import (
	"bytes"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBuildRemoteAddTLSOptions(t *testing.T) {
	if testing.Short() {
		t.Skip("live rootless remote ADD build")
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("registry credential sent to remote ADD source")
		}
		w.Header().Set("Last-Modified", time.Unix(123, 0).UTC().Format(http.TimeFormat))
		_, _ = io.WriteString(w, "CLI TLS payload")
	}))
	defer server.Close()
	certDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(certDir, "ca.crt"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name    string
		flags   []string
		allowed bool
	}{
		{"default strict", nil, false}, {"explicit strict", []string{"--tls-verify=true"}, false},
		{"insecure", []string{"--tls-verify=false"}, true}, {"custom CA", []string{"--cert-dir", certDir}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			store := maintenanceStoreOptions(filepath.Join(root, "store"))
			file := definitionFile(t, fmt.Sprintf("arg \"SOURCE_DATE_EPOCH\" \"metadata\"\nfrom \"scratch\" as=\"metadata\" {add %q \"/source\";}\n", server.URL+"/source"))
			cache := filepath.Join(root, "cache")
			for _, attempt := range []string{"cold", "warm"} {
				output := filepath.Join(root, attempt)
				args := []string{"--root", store.GraphRoot, "--runroot", store.RunRoot, "--storage-driver", "vfs", "build", file, "--output", output, "--creds", "registry-user:registry-secret", "--cache-from", "type=local,src=" + cache, "--cache-to", "type=local,dest=" + cache}
				args = append(args, test.flags...)
				var out, errs bytes.Buffer
				status := run(args, &out, &errs)
				if !test.allowed {
					if status == 0 || !strings.Contains(errs.String(), "x509") {
						t.Fatalf("expected strict certificate failure: status=%d stderr=%s", status, &errs)
					}
					break
				}
				if status != 0 {
					t.Fatalf("%s: status=%d stdout=%s stderr=%s", attempt, status, &out, &errs)
				}
				data, err := os.ReadFile(filepath.Join(output, "source"))
				if err != nil || string(data) != "CLI TLS payload" {
					t.Fatalf("%s output=%q error=%v", attempt, data, err)
				}
			}
		})
	}
}
