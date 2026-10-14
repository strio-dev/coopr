package buildah

import (
	"testing"

	"github.com/opencontainers/go-digest"
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

func TestReplaceCachedMetadataRetainsEveryLinkedInstructionLayer(t *testing.T) {
	const command = "COPY --link same /out/"
	// An identical previous instruction must stay in the caller prefix; matching
	// CreatedBy strings alone cannot identify the cached instruction's boundary.
	base := []v1.History{{CreatedBy: command}}
	history := append(append([]v1.History{}, base...), v1.History{CreatedBy: command}, v1.History{CreatedBy: command})
	builder := &upstream.Builder{OCIv1: v1.Image{History: history, RootFS: v1.RootFS{Type: "layers", DiffIDs: []digest.Digest{digest.FromString("base"), digest.FromString("inline"), digest.FromString("ordinary")}}}}
	builder.Docker.History = dockerHistory(history)
	changed, err := replaceCachedMetadata(builder, cachedMetadataReplacement{ParentDiffIDs: []digest.Digest{digest.FromString("base")}, OCIBase: base, DockerBase: dockerHistory(base)}, command)
	if err != nil || changed {
		t.Fatalf("linked cache history changed=%t, err=%v", changed, err)
	}
	if len(builder.OCIv1.History) != 3 || len(builder.Docker.History) != 3 {
		t.Fatal("linked cache history lost a layer or duplicated prior instruction")
	}
}

func TestReplaceCachedMetadataRejectsUnrelatedRootfsAndMissingLayers(t *testing.T) {
	const command = "COPY --link same /out/"
	for _, test := range []struct {
		name           string
		parent, cached []digest.Digest
		history        []v1.History
	}{
		{"different parent", []digest.Digest{digest.FromString("parent")}, []digest.Digest{digest.FromString("other"), digest.FromString("added")}, []v1.History{{CreatedBy: command}, {CreatedBy: command}}},
		{"shorter rootfs", []digest.Digest{digest.FromString("parent")}, nil, []v1.History{{CreatedBy: command, EmptyLayer: true}}},
		{"missing layer history", nil, []digest.Digest{digest.FromString("one"), digest.FromString("two")}, []v1.History{{CreatedBy: command}}},
		{"nonempty history without layer", nil, nil, []v1.History{{CreatedBy: command}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			builder := &upstream.Builder{OCIv1: v1.Image{History: test.history, RootFS: v1.RootFS{Type: "layers", DiffIDs: test.cached}}}
			builder.Docker.History = dockerHistory(test.history)
			if _, err := replaceCachedMetadata(builder, cachedMetadataReplacement{ParentDiffIDs: test.parent}, command); err == nil {
				t.Fatal("malformed cached lineage/history accepted")
			}
		})
	}
}
