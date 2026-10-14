//go:build !containers_image_sequoia

package buildah

import (
	"os"
	"strings"
	"testing"
)

func TestSequoiaSignerReportsMissingNativeBackend(t *testing.T) {
	_, cleanup, err := NativeSigners("", "fingerprint", nil, "", os.Stderr)
	cleanup()
	if err == nil || !strings.Contains(err.Error(), "Sequoia-PGP support is not enabled in this build") {
		t.Fatalf("got %v", err)
	}
}
