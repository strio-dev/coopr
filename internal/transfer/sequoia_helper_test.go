package transfer

import (
	"context"
	"encoding/json"
	"errors"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/strio-dev/coopr/internal/buildah"
	"github.com/strio-dev/coopr/internal/imagestore"
	"github.com/strio-dev/coopr/internal/localstore"
	imagecopy "go.podman.io/image/v5/copy"
)

func TestSequoiaHelperRoutesOnlyRequestedBackend(t *testing.T) {
	for _, signing := range []SigningOptions{{}, {SignBy: "gpg"}, {SigstoreParameterFile: "params"}, {SigstorePrivateKeyFile: "key"}} {
		if needsSequoiaHelper(Options{Signing: signing}) {
			t.Fatalf("unexpected helper route: %+v", signing)
		}
	}
	if needsSequoiaHelper(Options{Signing: SigningOptions{SequoiaFingerprint: "sq"}}) == buildah.NativeSequoia {
		t.Fatal("Sequoia capability guard incorrect")
	}
}

func TestSequoiaHelperDirectCopiesPreserveProtocolAndProtectSecrets(t *testing.T) {
	if buildah.NativeSequoia {
		t.Skip("tests static executable routing")
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 required for fake helper")
	}
	directory := t.TempDir()
	capture := filepath.Join(directory, "capture.json")
	script := `#!` + python + `
import json, os, sys
if sys.argv[1] == '--protocol-version':
 print('coopr-sequoia-signing/1')
 sys.exit(0)
p = sys.argv[1]
r = json.load(open(p))
assert os.stat(os.path.dirname(p)).st_mode & 0o777 == 0o700
assert os.stat(p).st_mode & 0o777 == 0o600
assert 'COSIGN_PASSWORD' not in os.environ
for key, expected in [('CredentialsFile', 'user:credential-secret'), ('CosignPasswordFile', 'cosign-secret\nraw')]:
 assert os.stat(r[key]).st_mode & 0o777 == 0o600
 assert open(r[key]).read() == expected
assert not r['Options']['Credentials']
assert not r['Options']['DecryptionKeys']
json.dump(r, open(` + strconvQuote(capture) + `, 'w'))
json.dump({'Reference':'helper-result'},open(r['ResultPath'],'w'))
`
	if err := os.WriteFile(filepath.Join(directory, buildah.SequoiaHelperName), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", directory)
	t.Setenv("COSIGN_PASSWORD", "cosign-secret\nraw")
	opts := Options{Credentials: "user:credential-secret", DecryptionKeys: []string{"key:inline-secret"}, Signing: SigningOptions{SequoiaFingerprint: "sq"}}
	source, root, _ := multiPlatformImageFixture(t, context.Background())
	layout := t.TempDir()
	if err := localstore.Put(context.Background(), layout, source, root, "helper:test"); err != nil {
		t.Fatal(err)
	}
	src, err := imagestore.LayoutReference(layout, root)
	if err != nil {
		t.Fatal(err)
	}
	calls := []struct {
		operation string
		call      func() (string, error)
	}{
		{"publish-reference", func() (string, error) {
			return publishImageReferenceSelection(context.Background(), src, root, "example.test/image", opts, imagecopy.CopySpecificImages)
		}},
		{"copy-destination", func() (string, error) {
			return copyImageDestination(context.Background(), layout, root, Destination{Transport: "dir", Name: "output"}, opts, nil)
		}},
		{"copy-archive", func() (string, error) {
			return copyImageArchive(context.Background(), layout, root, Destination{Transport: "oci-archive", Name: "output"}, opts, nil)
		}},
	}
	for _, call := range calls {
		t.Run(call.operation, func(t *testing.T) {
			result, err := call.call()
			if err != nil || result != "helper-result" {
				t.Fatalf("result=%s err=%v", result, err)
			}
			data, err := os.ReadFile(capture)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(data), "credential-secret") || strings.Contains(string(data), "cosign-secret") || strings.Contains(string(data), "inline-secret") {
				t.Fatal("secret serialized")
			}
			var request sequoiaTransferRequest
			if err := json.Unmarshal(data, &request); err != nil {
				t.Fatal(err)
			}
			if request.Operation != call.operation {
				t.Fatalf("operation=%s", request.Operation)
			}
			if call.operation == "publish-reference" && request.Selection != imagecopy.CopySpecificImages {
				t.Fatal("image selection changed")
			}
			if _, err := os.Stat(filepath.Dir(request.ResultPath)); !os.IsNotExist(err) {
				t.Fatalf("job was not cleaned: %v", err)
			}
		})
	}
}

func strconvQuote(value string) string { data, _ := json.Marshal(value); return string(data) }

func TestSequoiaHelperMixedPassphrases(t *testing.T) {
	file := filepath.Join(t.TempDir(), "gpg-password")
	if err := os.WriteFile(file, []byte("sq-pass\n"), 0600); err != nil {
		t.Fatal(err)
	}
	raw := []byte("cosign\nraw")
	options := SigningOptions{SequoiaFingerprint: "sq", SigstoreParameterFile: "params", PassphraseFile: file, sigstorePassword: &raw}
	sigstore, gpg, err := options.passphrases()
	if err != nil || gpg != "sq-pass" || string(sigstore) != string(raw) {
		t.Fatalf("mixed passphrases were not independent: %v", err)
	}
}

func TestSequoiaHelperTransferCancellationCleansSecrets(t *testing.T) {
	if buildah.NativeSequoia {
		t.Skip("static routing test")
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("fake helper requires python3")
	}
	directory := t.TempDir()
	marker := filepath.Join(directory, "request-path")
	script := `#!` + python + `
import sys, time
if sys.argv[1] == '--protocol-version':
 print('coopr-sequoia-signing/1')
 sys.exit(0)
open(` + strconvQuote(marker) + `,'w').write(sys.argv[1])
time.sleep(60)
`
	if err := os.WriteFile(filepath.Join(directory, buildah.SequoiaHelperName), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", directory)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err = transferWithSequoiaHelper(ctx, "copy-archive", "unused", v1.Descriptor{}, Destination{}, Options{Credentials: "user:secret"}, false, imagecopy.CopyAllImages)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancel error: %v", err)
	}
	path, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Dir(string(path))); !os.IsNotExist(err) {
		t.Fatalf("cancelled helper job survived: %v", err)
	}
}

func TestSequoiaHelperCleanupFailurePreservesPublishedReference(t *testing.T) {
	if buildah.NativeSequoia {
		t.Skip("static routing test")
	}
	defer withoutDACOverrides(t)()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("fake helper requires python3")
	}
	directory := t.TempDir()
	marker := filepath.Join(directory, "job-directory")
	script := `#!` + python + `
import json, os, sys
if sys.argv[1] == '--protocol-version':
 print('coopr-sequoia-signing/1')
 sys.exit(0)
request = json.load(open(sys.argv[1]))
job = os.path.dirname(sys.argv[1])
open(` + strconvQuote(marker) + `, 'w').write(job)
with open(request['ResultPath'], 'w') as result:
 json.dump({'Reference': 'published-reference'}, result)
os.chmod(job, 0o500)
`
	if err := os.WriteFile(filepath.Join(directory, buildah.SequoiaHelperName), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", directory)
	t.Cleanup(func() {
		job, err := os.ReadFile(marker)
		if err != nil {
			t.Errorf("read cleanup marker: %v", err)
			return
		}
		if err := os.Chmod(string(job), 0700); err != nil {
			t.Errorf("restore job directory permissions: %v", err)
		}
		if err := os.RemoveAll(string(job)); err != nil {
			t.Errorf("remove regression job directory: %v", err)
		}
	})
	reference, err := transferWithSequoiaHelper(context.Background(), "publish-reference", "unused", v1.Descriptor{}, Destination{}, Options{Credentials: "user:secret"}, false, imagecopy.CopyAllImages)
	if reference != "published-reference" {
		t.Fatalf("published reference lost: %q", reference)
	}
	if !errors.Is(err, os.ErrPermission) {
		t.Fatalf("cleanup permission failure was not reported: %v", err)
	}
}
