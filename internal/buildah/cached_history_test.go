package buildah

import (
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/coopr/internal/definition"
	"github.com/strio-dev/coopr/internal/planner"
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

// Buildah deliberately omits all history when a nonempty base has no history.
// Restoration must preserve that valid OCI representation, not invent ancestry.
func TestReplaceCachedMetadataPreservesOmittedBaseHistory(t *testing.T) {
	parent := digest.FromString("unrecorded-base")
	for _, emptyRun := range []bool{false, true} {
		t.Run(map[bool]string{false: "filesystem change", true: "empty RUN"}[emptyRun], func(t *testing.T) {
			diffs := []digest.Digest{parent}
			if !emptyRun {
				diffs = append(diffs, digest.FromString("run-output"))
			}
			builder := &upstream.Builder{OCIv1: v1.Image{RootFS: v1.RootFS{Type: "layers", DiffIDs: diffs}}}
			replacement := cachedMetadataReplacement{ParentDiffIDs: []digest.Digest{parent}, Pending: []v1.History{{CreatedBy: "ENV CURRENT=yes", EmptyLayer: true}}}
			changed, err := replaceCachedMetadata(builder, replacement, "RUN test -s /payload")
			if err != nil || changed {
				t.Fatalf("omitted history = changed:%v, error:%v", changed, err)
			}
			if len(builder.OCIv1.History) != 0 || len(builder.Docker.History) != 0 {
				t.Fatal("restoration invented missing history")
			}
		})
	}
}

func TestReplaceCachedMetadataRejectsUnexpectedOmittedHistory(t *testing.T) {
	parent := digest.FromString("base")
	for _, test := range []struct {
		name          string
		parent        []digest.Digest
		base, cached  []v1.History
		dockerMissing bool
	}{
		{name: "scratch"},
		{name: "recorded parent", parent: []digest.Digest{parent}, base: []v1.History{{CreatedBy: "COPY base /"}}},
		{name: "metadata-only parent history", parent: []digest.Digest{parent}, base: []v1.History{{CreatedBy: "ENV X=yes", EmptyLayer: true}}},
		{name: "only Docker history missing", parent: []digest.Digest{parent}, cached: []v1.History{{CreatedBy: "RUN true"}}, dockerMissing: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			diffs := append(append([]digest.Digest{}, test.parent...), digest.FromString("result"))
			builder := &upstream.Builder{OCIv1: v1.Image{History: test.cached, RootFS: v1.RootFS{Type: "layers", DiffIDs: diffs}}}
			if !test.dockerMissing {
				builder.Docker.History = dockerHistory(test.cached)
			}
			_, err := replaceCachedMetadata(builder, cachedMetadataReplacement{ParentDiffIDs: test.parent, OCIBase: test.base, DockerBase: dockerHistory(test.base)}, "RUN true")
			if err == nil {
				t.Fatal("unexpected omitted instruction history accepted")
			}
		})
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

func TestCanonicalRunMountHistoryRetainsStrictInstructionChecks(t *testing.T) {
	makeInput := func(selector string) instructionCacheInput {
		mount := definition.Instruction{Name: "mount", Arguments: []string{"bind"}, Properties: map[string]string{"from": selector, "source": "/selected", "target": "/input", "readonly": "true"}}
		input := instructionCacheTestInput(planner.Operation{Instruction: definition.Instruction{Name: "run", Arguments: []string{"cat /input/payload"}, Children: []definition.Instruction{mount}}})
		input.ResolvedInputsComplete = true
		input.ResolvedInputs = []instructionCacheResolvedInput{{Kind: "image-rootfs", Identity: digest.FromString("verified-filesystem").String()}}
		return input
	}
	history := func(input instructionCacheInput) string {
		t.Helper()
		operation := canonicalInstructionCacheOperation(input.Operation, input.ResolvedInputs, input.ResolvedInputsComplete)
		createdBy, ok := plannedOperationHistory(operation, nil)
		if !ok {
			t.Fatal("instruction history missing")
		}
		return createdBy
	}
	cold := history(makeInput("containers-storage:first"))
	warm := history(makeInput("containers-storage:second"))
	if cold != warm || !strings.Contains(cold, "from=resolved:image-rootfs:sha256:") {
		t.Fatalf("history does not identify stable verified filesystem: %q / %q", cold, warm)
	}
	check := func(expected string) error {
		builder := &upstream.Builder{OCIv1: v1.Image{History: []v1.History{{CreatedBy: cold, EmptyLayer: true}}}}
		builder.Docker.History = dockerHistory(builder.OCIv1.History)
		_, err := replaceCachedMetadata(builder, cachedMetadataReplacement{}, expected)
		return err
	}
	if err := check(warm); err != nil {
		t.Fatalf("equivalent mount history rejected: %v", err)
	}
	for name, change := range map[string]func(*instructionCacheInput){
		"command":  func(v *instructionCacheInput) { v.Operation.Arguments = []string{"false"} },
		"path":     func(v *instructionCacheInput) { v.Operation.Children[0].Properties["source"] = "/other" },
		"readonly": func(v *instructionCacheInput) { v.Operation.Children[0].Properties["readonly"] = "false" },
		"filesystem": func(v *instructionCacheInput) {
			v.ResolvedInputs[0].Identity = digest.FromString("other-filesystem").String()
		},
		"incomplete": func(v *instructionCacheInput) { v.ResolvedInputsComplete = false },
	} {
		t.Run(name, func(t *testing.T) {
			input := makeInput("containers-storage:second")
			change(&input)
			if err := check(history(input)); err == nil {
				t.Fatal("incompatible cached instruction history accepted")
			}
		})
	}
}
