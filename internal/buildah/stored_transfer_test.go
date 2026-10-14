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
			name:    "image and manifest list",
			options: StoredTransferOptions{ImageID: "image", ManifestList: "list", Destination: "oci:output"},
			want:    "requires exactly one source",
		},
		{
			name:    "index and manifest list",
			options: StoredTransferOptions{Index: index, ManifestList: "list", Destination: "oci:output"},
			want:    "requires exactly one source",
		},
		{
			name:    "manifest list without native destination",
			options: StoredTransferOptions{ManifestList: "list", RegistryDestination: "example.com/list:test"},
			want:    "manifest-list transfer requires a native push destination",
		},
		{
			name:    "manifest list selected instance",
			options: StoredTransferOptions{ManifestList: "list", Destination: "oci:output", RegistryInstance: true},
			want:    "manifest-list transfer requires a native push destination",
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

func TestStoredManifestTransferSelectionSurvivesWorkerJSON(t *testing.T) {
	for _, all := range []bool{false, true} {
		original := StoredTransferOptions{ManifestList: "local-list", ManifestListAll: all, Destination: "oci:/tmp/output:test"}
		data, err := json.Marshal(storedTransferRequest{Options: original})
		if err != nil {
			t.Fatal(err)
		}
		var decoded storedTransferRequest
		if err := json.Unmarshal(data, &decoded); err != nil {
			t.Fatal(err)
		}
		if decoded.Options.ManifestList != original.ManifestList || decoded.Options.ManifestListAll != all || decoded.Options.Destination != original.Destination {
			t.Fatalf("manifest worker options changed: %+v", decoded.Options)
		}
	}
}

func TestStoredArchiveCompressionOptionSurvivesWorkerJSON(t *testing.T) {
	for _, value := range []*bool{nil, new(false), new(true)} {
		for _, destination := range []string{"oci-archive:/tmp/output.tar:test", "oci:/tmp/output:test", "dir:/tmp/output"} {
			data, err := json.Marshal(storedTransferRequest{Options: StoredTransferOptions{Destination: destination, ArchiveUncompressed: value}})
			if err != nil {
				t.Fatal(err)
			}
			var decoded storedTransferRequest
			if err := json.Unmarshal(data, &decoded); err != nil {
				t.Fatal(err)
			}
			system, err := storedTransferSystemContext(decoded.Options, "")
			if err != nil {
				t.Fatal(err)
			}
			want := (strings.HasPrefix(destination, "oci-archive:") || strings.HasPrefix(destination, "oci:")) && value != nil && *value
			if system.OCIAcceptUncompressedLayers != want {
				t.Fatalf("archive option after IPC for %s = %t, want %t", destination, system.OCIAcceptUncompressedLayers, want)
			}
			if (decoded.Options.ArchiveUncompressed == nil) != (value == nil) {
				t.Fatal("worker lost explicit archive option presence")
			}
		}
	}
}
