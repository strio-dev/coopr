//go:build cosignintegration

package transfer

import (
	"context"
	"os/exec"
	"testing"
	"time"
)

func TestCopyRootSigstoreSignatureWithCosign(t *testing.T) {
	if testing.Short() {
		t.Skip("external Cosign verification")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, result, publicKey, _ := signedRegistryImageFixture(t, ctx)
	command := exec.CommandContext(ctx, "cosign", "verify", "--key", publicKey, "--insecure-ignore-tlog", "--allow-http-registry", result)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("cosign verify %s: %v\n%s", result, err, output)
	}
}
