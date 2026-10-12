package buildah

import (
	"bytes"
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"github.com/distribution/reference"
	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
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

func TestStoredTransferProgressRequest(t *testing.T) {
	var progress bytes.Buffer
	request := storedTransferRequest{Options: StoredTransferOptions{ProgressWriter: &progress}, Progress: true}
	data, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	var decoded storedTransferRequest
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if !decoded.Progress || decoded.Options.ProgressWriter != nil {
		t.Fatalf("worker request = %+v", decoded)
	}
	if bytes.Contains(data, []byte("ProgressWriter")) {
		t.Fatalf("serialized process-local writer: %s", data)
	}
}

func TestStoredTransferRejectsConflictingSources(t *testing.T) {
	root := digest.FromString("selected index")
	index := &StoredIndexSnapshot{Root: v1.Descriptor{Digest: root}}
	for _, test := range []struct {
		name    string
		options StoredTransferOptions
		want    string
	}{
		{
			name:    "missing source",
			options: StoredTransferOptions{Destination: "dir:output"},
			want:    "requires exactly one source",
		},
		{
			name:    "both sources",
			options: StoredTransferOptions{ImageID: "native-image", Index: index, Destination: "dir:output"},
			want:    "requires exactly one source",
		},
		{
			name:    "local index signing",
			options: StoredTransferOptions{Index: index, LocalName: "example:latest", SignBy: "key"},
			want:    "cannot sign an index snapshot locally",
		},
		{
			name:    "mismatched expected root",
			options: StoredTransferOptions{Index: index, ExpectedManifest: digest.FromString("different index"), Destination: "dir:output"},
			want:    "index snapshot is " + root.String() + ", want ",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := TransferStoredImageResultSupervised(context.Background(), test.options)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("transfer error = %v, want %q", err, test.want)
			}
		})
	}
}
