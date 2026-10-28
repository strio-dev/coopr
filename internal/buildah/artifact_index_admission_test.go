package buildah

import (
	"io"
	"log"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/registry"
)

func TestArtifactIndexNewerDoesNotHidePostPullAdmissionFailure(t *testing.T) {
	server := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(server.Close)
	repository := artifactIndexTestRepository(t, server.URL)
	reference := repository.Reference.String() + ":latest"
	store, system := artifactSourceTestStore(t)
	previous := artifactSourceTestAdd(t, t.Context(), store, reference, map[string]string{"payload": "previous flat"})
	children := artifactIndexTestPublish(t, repository, "updated")
	// Cached flat admission does not need index metadata, but publishing the
	// newly pulled index must acquire this lock before it can replace that input.
	if err := os.MkdirAll(filepath.Join(artifactIndexDirectory(store.GraphRoot()), ".coopr-store.lock"), 0o700); err != nil {
		t.Fatal(err)
	}
	selected, err := artifactIndexTestResolve(t, reference, "amd64", "newer", store, system)
	if err == nil {
		t.Fatalf("newer hid a post-pull metadata failure: selected %s, cached %s, pulled %s", selected.Selected.Digest, previous.Digest, children["amd64"].Digest)
	}
	if !strings.Contains(err.Error(), "retain artifact index") {
		t.Fatalf("failure did not come from post-pull index admission: %v", err)
	}
	// The child landed successfully in the native artifact store. Only its
	// subsequent metadata admission failed; transport fallback must not hide it.
	childReference := repository.Reference.String() + "@" + children["amd64"].Digest.String()
	pulled, err := artifactIndexTestResolve(t, childReference, "amd64", "never", store, system)
	if err != nil || pulled.Selected.Digest != children["amd64"].Digest {
		t.Fatalf("native Pull had not completed before metadata failure: %+v, %v", pulled, err)
	}
	artifactIndexTestPayload(t, store, pulled, "updated-amd64")
}
