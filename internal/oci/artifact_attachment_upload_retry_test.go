package oci

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	registryserver "github.com/google/go-containerregistry/pkg/registry"
)

func TestArtifactRegistryAttachmentUploadRetry(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodPut} {
		for _, test := range []struct {
			persistent bool
			retries    uint
		}{
			{retries: 0},
			{retries: 1},
			{persistent: true, retries: 1},
		} {
			t.Run(fmt.Sprintf("%s/persistent=%t/retries=%d", method, test.persistent, test.retries), func(t *testing.T) {
				backend := registryserver.New()
				var armed atomic.Bool
				var attempts atomic.Int64
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
					if request.Method == http.MethodPut && strings.Contains(request.URL.Path, "/manifests/") {
						data, err := io.ReadAll(request.Body)
						if err != nil {
							t.Error(err)
							return
						}
						request.Body = io.NopCloser(bytes.NewReader(data))
						var envelope struct {
							ArtifactType string `json:"artifactType"`
						}
						if err := json.Unmarshal(data, &envelope); err != nil {
							t.Error(err)
							return
						}
						if envelope.ArtifactType == ComponentArtifactType {
							backend.ServeHTTP(w, request)
							armed.Store(true)
							return
						}
					}
					if armed.Load() && request.Method == method && strings.Contains(request.URL.Path, "/blobs/uploads/") {
						attempt := attempts.Add(1)
						if test.persistent || attempt == 1 {
							http.Error(w, "temporary attachment upload failure", http.StatusServiceUnavailable)
							return
						}
						armed.Store(false)
					}
					backend.ServeHTTP(w, request)
				}))
				t.Cleanup(server.Close)
				resolver, err := testResolver(t, Options{TLSVerify: boolOption(false), RetrySet: true, Retry: test.retries, RetryDelay: time.Millisecond})
				if err != nil {
					t.Fatal(err)
				}
				metadata, paths := baleComponentMetadata(t, "amd64", "attachment-upload-retry", baleRegistryDependency)
				reference := strings.TrimPrefix(server.URL, "http://") + "/coopr/attachment-upload:stable"
				_, err = resolver.PublishComponent(t.Context(), reference, metadata, paths)
				if attempts.Load() != int64(test.retries+1) {
					t.Fatalf("attachment %s attempts=%d want%d", method, attempts.Load(), test.retries+1)
				}
				if test.persistent || test.retries == 0 {
					if err == nil {
						t.Fatal("attachment upload succeeded after exhausted retry budget")
					}
					return
				}
				if err != nil {
					t.Fatalf("attachment upload did not recover: %v", err)
				}
			})
		}
	}
}
