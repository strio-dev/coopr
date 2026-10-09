package buildah

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func gitSSHFixture(t *testing.T) (source, privateKey string, knownHosts []byte) {
	t.Helper()
	root := t.TempDir()
	working := filepath.Join(root, "working")
	runGit(t, "init", "-b", "main", working)
	runGit(t, "-C", working, "config", "user.name", "Coopr Test")
	runGit(t, "-C", working, "config", "user.email", "coopr@example.invalid")
	if err := os.WriteFile(filepath.Join(working, "proof"), []byte("ssh context\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, "-C", working, "add", "proof")
	runGit(t, "-C", working, "commit", "-m", "fixture")
	repository := filepath.Join(root, "repository.git")
	runGit(t, "clone", "--bare", working, repository)

	privateKey = filepath.Join(root, "id_ed25519")
	hostKey := filepath.Join(root, "ssh_host_ed25519_key")
	runCommand(t, "ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", privateKey)
	runCommand(t, "ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", hostKey)

	helper, err := runtimeFixtureBinary("sshgitserver")
	if err != nil {
		t.Fatal(err)
	}
	if helper == "" {
		helper = filepath.Join(root, "sshgitserver")
		build := exec.Command("go", "build", "-trimpath", "-o", helper, "./testdata/sshgitserver")
		build.Dir = packageDirectory(t)
		if output, err := build.CombinedOutput(); err != nil {
			t.Fatalf("build SSH Git fixture server: %v\n%s", err, output)
		}
	}

	serverContext, cancel := context.WithCancel(context.Background())
	command := exec.CommandContext(serverContext, helper,
		"-repository", repository,
		"-client-public-key", privateKey+".pub",
		"-host-private-key", hostKey,
	)
	stdout, err := command.StdoutPipe()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		if err := command.Wait(); err != nil && serverContext.Err() == nil {
			t.Errorf("wait for SSH Git fixture server: %v\n%s", err, stderr.String())
		}
	})

	ready := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		if scanner.Scan() {
			ready <- scanner.Text()
			return
		}
		ready <- ""
	}()
	var address string
	select {
	case address = <-ready:
	case <-time.After(30 * time.Second):
		t.Fatal("SSH Git fixture server did not report its listener")
	}
	if address == "" {
		t.Fatalf("SSH Git fixture server exited before listening: %s", stderr.String())
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		t.Fatal(err)
	}
	hostPublicKey, err := os.ReadFile(hostKey + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(string(hostPublicKey))
	if len(fields) < 2 {
		t.Fatalf("invalid SSH host public key %q", hostPublicKey)
	}
	knownHosts = []byte(fmt.Sprintf("[%s]:%s %s %s\n", host, port, fields[0], fields[1]))
	return "ssh://git@" + address + repository, privateKey, knownHosts
}

func packageDirectory(t *testing.T) string {
	t.Helper()
	directory, err := os.Getwd()
	if err != nil {
		t.Fatalf("locate buildah test package: %v", err)
	}
	return directory
}

func TestBuildPlanAddsSSHGitSourceWithPinnedHostKey(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH=1 for a live supervised SSH Git ADD build")
	}
	source, privateKey, knownHosts := gitSSHFixture(t)
	root := t.TempDir()
	knownHostsPath := filepath.Join(root, "known_hosts")
	if err := os.WriteFile(knownHostsPath, knownHosts, 0o600); err != nil {
		t.Fatal(err)
	}
	layout := filepath.Join(root, "layout")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	plan := testPlan(t, "from \"scratch\"\nadd \""+source+"#main\" \"/source/\"\n")
	if _, err := BuildPlanSupervised(ctx, plan, SupervisedPlanOptions{
		Store: cacheTestStore(root), ContextDir: root, Isolation: "rootless", Runtime: "crun", Output: Output{Path: layout},
		Secrets: []string{"id=GIT_KNOWN_HOSTS.127.0.0.1,src=" + knownHostsPath},
		SSH:     []string{"default=" + privateKey},
	}); err != nil {
		t.Fatal(err)
	}
	manifest, _ := readPlanImage(t, layout)
	if len(manifest.Layers) != 1 {
		t.Fatalf("SSH Git ADD layers = %d, want 1", len(manifest.Layers))
	}
	blob := filepath.Join(layout, "blobs", "sha256", manifest.Layers[0].Digest.Encoded())
	if got := readLayerFile(t, blob, "source/proof"); got != "ssh context\n" {
		t.Fatalf("SSH Git ADD proof = %q", got)
	}
}
