package buildah

import (
	"testing"

	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	upstream "go.podman.io/buildah"
)

func TestReplaceCachedMetadataRetainsEmptyRun(t *testing.T) {
	base := []v1.History{{CreatedBy: "COPY payload /payload"}}
	run := v1.History{CreatedBy: "RUN --network=none test -s /payload", EmptyLayer: true}
	history := append(base, run)
	builder := &upstream.Builder{OCIv1: v1.Image{History: history}}
	builder.Docker.History = dockerHistory(history)
	changed, err := replaceCachedMetadata(builder, cachedMetadataReplacement{OCIBase: base, DockerBase: dockerHistory(base)}, run.CreatedBy)
	if err != nil || changed {
		t.Fatalf("unchanged empty RUN history = changed:%v, error:%v", changed, err)
	}
	if !builder.OCIv1.History[1].EmptyLayer || !builder.Docker.History[1].EmptyLayer {
		t.Fatal("empty RUN acquired a filesystem layer")
	}
}

func TestReplaceCachedMetadataRejectsWrongInstruction(t *testing.T) {
	const expected = "RUN --network=none test -s /payload"
	for _, test := range []struct {
		name, ociCommand, dockerCommand string
		ociEmpty, dockerEmpty           bool
	}{
		{"metadata tail", "LABEL stale=yes", "LABEL stale=yes", true, true},
		{"different RUN", "RUN false", "RUN false", true, true},
		{"different Docker history", expected, "COPY payload /payload", true, false},
		{"inconsistent layer flags", expected, expected, true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			builder := &upstream.Builder{OCIv1: v1.Image{History: []v1.History{{CreatedBy: test.ociCommand, EmptyLayer: test.ociEmpty}}}}
			builder.Docker.History = dockerHistory([]v1.History{{CreatedBy: test.dockerCommand, EmptyLayer: test.dockerEmpty}})
			if _, err := replaceCachedMetadata(builder, cachedMetadataReplacement{}, expected); err == nil {
				t.Fatal("invalid terminal cache history was accepted")
			}
		})
	}
}
