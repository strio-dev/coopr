package transfer

import (
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestValidateImportedImageIndexRejectsAuxiliaryAndDuplicateImages(t *testing.T) {
	linuxAMD64 := v1.Platform{OS: "linux", Architecture: "amd64"}
	image := func(name string, platform v1.Platform) v1.Descriptor {
		return v1.Descriptor{MediaType: v1.MediaTypeImageManifest, Digest: digest.FromString(name), Size: 1, Platform: &platform}
	}
	valid := v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, Manifests: []v1.Descriptor{
		image("amd64", linuxAMD64), image("arm64", v1.Platform{OS: "linux", Architecture: "arm64"}),
	}}
	if err := validateImportedImageIndex(valid); err != nil {
		t.Fatalf("valid runnable index rejected: %v", err)
	}

	attestationPlatform := v1.Platform{OS: "unknown", Architecture: "unknown"}
	attestation := image("attestation", attestationPlatform)
	attestation.Annotations = map[string]string{"vnd.docker.reference.type": "attestation-manifest"}
	withAttestation := valid
	withAttestation.Manifests = append(append([]v1.Descriptor(nil), valid.Manifests...), attestation)
	if err := validateImportedImageIndex(withAttestation); err == nil || !strings.Contains(err.Error(), "auxiliary descriptor") {
		t.Fatalf("modern attestation shape = %v", err)
	}

	duplicate := valid
	duplicate.Manifests = []v1.Descriptor{image("first-amd64", linuxAMD64), image("second-amd64", linuxAMD64)}
	if err := validateImportedImageIndex(duplicate); err == nil || !strings.Contains(err.Error(), "multiple images for platform linux/amd64") {
		t.Fatalf("duplicate platform shape = %v", err)
	}
}
