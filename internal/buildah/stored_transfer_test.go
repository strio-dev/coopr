package buildah

import (
	"net/url"
	"testing"

	"github.com/distribution/reference"
)

func TestStoredSignatureTopLevelPreservesSharedURL(t *testing.T) {
	named, err := reference.ParseNormalizedNamed("registry.example/team/image:latest")
	if err != nil {
		t.Fatal(err)
	}
	base, err := url.Parse("https://signatures.example/lookaside/team/image?scope=repository#stored")
	if err != nil {
		t.Fatal(err)
	}
	before := *base
	got, err := storedSignatureTopLevel(base, named)
	if err != nil {
		t.Fatal(err)
	}
	if want := "https://signatures.example/lookaside?scope=repository#stored"; got != want {
		t.Fatalf("signature top level = %q, want %q", got, want)
	}
	if *base != before {
		t.Fatalf("shared signature URL changed from %q to %q", before.String(), base.String())
	}
}
