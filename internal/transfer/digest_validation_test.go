package transfer

import (
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	"github.com/strio-dev/coopr/internal/oci"
)

func TestLocalSelectorRejectsMalformedSHA256Digests(t *testing.T) {
	for _, kind := range []oci.Kind{oci.Image, oci.Component} {
		for _, value := range []string{"sha256:", "sha256:bad", "sha256:" + strings.Repeat("g", 64)} {
			t.Run(string(kind)+"/"+value, func(t *testing.T) {
				if _, err := localSelector(value, kind); err == nil || !strings.Contains(err.Error(), "invalid local digest") {
					t.Fatalf("error = %v, want invalid local digest", err)
				}
			})
		}
		valid := digest.FromString("local selector").String()
		if got, err := localSelector(valid, kind); err != nil || got != valid {
			t.Fatalf("valid local selector = %q, %v, want %q", got, err, valid)
		}
	}
}
