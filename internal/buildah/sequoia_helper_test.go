package buildah

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSequoiaHelperDiscovery(t *testing.T) {
	directory := t.TempDir()
	t.Setenv("PATH", directory)
	if _, err := FindSequoiaHelper(context.Background()); err == nil || !strings.Contains(err.Error(), "optional") {
		t.Fatalf("missing helper: %v", err)
	}
	helper := filepath.Join(directory, SequoiaHelperName)
	for _, test := range []struct {
		name, output string
		valid        bool
	}{{"incompatible", "bad", false}, {"installed", SequoiaHelperProtocol, true}} {
		t.Run(test.name, func(t *testing.T) {
			if err := os.WriteFile(helper, []byte("#!/bin/sh\nprintf '%s\\n' '"+test.output+"'\n"), 0700); err != nil {
				t.Fatal(err)
			}
			path, err := FindSequoiaHelper(context.Background())
			if test.valid && (err != nil || path != helper) {
				t.Fatalf("path=%s err=%v", path, err)
			}
			if !test.valid && err == nil {
				t.Fatal("accepted incompatible helper")
			}
		})
	}
}

func TestSequoiaHelperHandshakeCancellation(t *testing.T) {
	directory := t.TempDir()
	t.Setenv("PATH", directory)
	if err := os.WriteFile(filepath.Join(directory, SequoiaHelperName), []byte("#!/bin/sh\nwhile :; do :; done\n"), 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	if _, err := FindSequoiaHelper(ctx); err == nil {
		t.Fatal("accepted hanging helper")
	}
	if time.Since(started) > 3*time.Second {
		t.Fatal("handshake cancellation was not bounded")
	}
}

func TestSigningWorkerSecretFilesAndJSON(t *testing.T) {
	directory := t.TempDir()
	secret := []byte("raw\npassword\r")
	path, err := StageSigningSecret(directory, "password", secret)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("info=%v err=%v", info, err)
	}
	value, err := os.ReadFile(path)
	if err != nil || string(value) != string(secret) {
		t.Fatal("secret bytes changed")
	}
	message := filepath.Join(directory, "message")
	if err := WriteSigningWorkerJSON(message, map[string]string{"password_file": path}); err != nil {
		t.Fatal(err)
	}
	info, err = os.Stat(message)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("message mode: %v %v", info, err)
	}
	data, _ := os.ReadFile(message)
	if strings.Contains(string(data), "password\\r") {
		t.Fatal("secret in JSON")
	}
	for _, malformed := range []string{`{"password_file":"x","unknown":1}`, `{"password_file":"x"} {}`, `{`} {
		if err := os.WriteFile(message, []byte(malformed), 0600); err != nil {
			t.Fatal(err)
		}
		var request struct {
			PasswordFile string `json:"password_file"`
		}
		if err := ReadSigningWorkerJSON(message, &request); err == nil {
			t.Fatal("accepted malformed protocol")
		}
	}
}

func TestSequoiaStoredTransferUsesOptionalHelper(t *testing.T) {
	if NativeSequoia {
		t.Skip("static routing test")
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("fake helper requires python3")
	}
	directory := t.TempDir()
	script := `#!` + python + `
import json, os, sys
if sys.argv[1] == '--protocol-version':
 print('coopr-sequoia-signing/1')
 sys.exit(0)
r = json.load(open(sys.argv[1]))
assert r['protocol'] == 'coopr-sequoia-signing/1'
assert not r['options']['Credentials']
assert os.stat(r['credentials_file']).st_mode & 0o777 == 0o600
assert open(r['credentials_file']).read() == 'user:stored-credential-secret'
assert open(r['cosign_password_file']).read() == 'raw\ncosign'
assert 'COSIGN_PASSWORD' not in os.environ
json.dump({'reference':'stored-helper-result'},open(r['result_path'],'w'))
`
	if err := os.WriteFile(filepath.Join(directory, SequoiaHelperName), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", directory)
	t.Setenv("COSIGN_PASSWORD", "raw\ncosign")
	result, err := TransferStoredImageResultSupervised(context.Background(), StoredTransferOptions{ImageID: "unused-native-image", Destination: "dir:unused-output", SequoiaFingerprint: "sq", Credentials: "user:stored-credential-secret"})
	if err != nil || result.Reference != "stored-helper-result" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestSequoiaStoredCleanupFailurePreservesPublishedResult(t *testing.T) {
	if NativeSequoia {
		t.Skip("static routing test")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory write permissions")
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("fake helper requires python3")
	}
	directory := t.TempDir()
	marker := filepath.Join(directory, "job-directory")
	manifestDigest := "sha256:" + strings.Repeat("a", 64)
	script := `#!` + python + `
import json, os, sys
if sys.argv[1] == '--protocol-version':
 print('coopr-sequoia-signing/1')
 sys.exit(0)
request = json.load(open(sys.argv[1]))
job = os.path.dirname(sys.argv[1])
open('` + marker + `', 'w').write(job)
with open(request['result_path'], 'w') as result:
 json.dump({'reference': 'published-reference', 'manifest': {'digest': '` + manifestDigest + `', 'size': 123}, 'manifest_data': 'e30='}, result)
os.chmod(job, 0o500)
`
	if err := os.WriteFile(filepath.Join(directory, SequoiaHelperName), []byte(script), 0700); err != nil {
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
	result, err := TransferStoredImageResultSupervised(context.Background(), StoredTransferOptions{ImageID: "unused-native-image", Destination: "dir:unused-output", SequoiaFingerprint: "sq", Credentials: "user:secret"})
	if result.Reference != "published-reference" || result.Manifest.Digest.String() != manifestDigest || result.Manifest.Size != 123 || string(result.ManifestData) != "{}" {
		t.Fatalf("published result lost: %+v", result)
	}
	if !errors.Is(err, os.ErrPermission) {
		t.Fatalf("cleanup permission failure was not reported: %v", err)
	}
}
