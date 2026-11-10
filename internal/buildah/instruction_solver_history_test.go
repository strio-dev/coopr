package buildah

import (
	"slices"
	"testing"
	"time"

	digest "github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	upstream "go.podman.io/buildah"
)

func TestGraphCachedHistoryUsesSelectedWholeAncestry(t *testing.T) {
	oldTime := time.Unix(10, 0).UTC()
	currentTime := time.Unix(20, 0).UTC()
	source := v1.History{Created: &oldTime, CreatedBy: "COPY input /input"}
	run := v1.History{CreatedBy: "RUN compile"}
	selectedDiffs := []digest.Digest{digest.FromString("old complete COPY"), digest.FromString("old complete RUN")}
	builder := &upstream.Builder{OCIv1: v1.Image{RootFS: v1.RootFS{Type: "layers", DiffIDs: slices.Clone(selectedDiffs)}, History: []v1.History{source, run}}}
	builder.Docker.History = dockerHistory(builder.OCIv1.History)
	currentHistory := []v1.History{{Created: &currentTime, CreatedBy: "COPY input /input"}}
	changed, err := replaceGraphCachedMetadata(builder, cachedMetadataReplacement{ParentDiffIDs: []digest.Digest{digest.FromString("new physical COPY")}, OCIBase: currentHistory, DockerBase: dockerHistory(currentHistory)}, run.CreatedBy, 1)
	if err != nil || changed {
		t.Fatalf("whole result restore changed=%v error=%v", changed, err)
	}
	if !slices.Equal(builder.OCIv1.RootFS.DiffIDs, selectedDiffs) {
		t.Fatal("grafted cached RUN onto current physical COPY")
	}
	if !builder.OCIv1.History[0].Created.Equal(oldTime) {
		t.Fatal("selected physical history replaced with unrelated physical parent")
	}
}

func TestGraphCachedHistoryRejectsUnboundLayerShape(t *testing.T) {
	builder := &upstream.Builder{OCIv1: v1.Image{RootFS: v1.RootFS{Type: "layers", DiffIDs: []digest.Digest{digest.FromString("one"), digest.FromString("two")}}, History: []v1.History{{CreatedBy: "COPY input /input"}, {CreatedBy: "RUN compile"}}}}
	builder.Docker.History = dockerHistory(builder.OCIv1.History)
	if _, err := replaceGraphCachedMetadata(builder, cachedMetadataReplacement{}, "RUN compile", 1); err == nil {
		t.Fatal("accepted current history without selected parent layer")
	}
}

func TestGraphCachedHistoryPreservesAuthoredCommonAncestry(t *testing.T) {
	requireLiveInstructionCache(t)
	for _, changedTail := range []bool{false, true} {
		name := "identical ancestry"
		if changedTail {
			name = "common base with changed COPY"
		}
		t.Run(name, func(t *testing.T) {
			lease, err := acquireStore(cacheTestStore(t.TempDir()))
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := lease.Close(); err != nil {
					t.Error(err)
				}
			}()
			builder, err := upstream.NewBuilder(t.Context(), lease.store, upstream.BuilderOptions{FromImage: "scratch"})
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := builder.Delete(); err != nil {
					t.Error(err)
				}
			}()
			oldTime, currentTime := time.Unix(10, 0).UTC(), time.Unix(20, 0).UTC()
			oldBase := v1.History{Created: &oldTime, CreatedBy: "old base command", Author: "old author"}
			currentBase := v1.History{Created: &currentTime, CreatedBy: "current base command", Author: "current author"}
			selectedDiffs := []digest.Digest{digest.FromString("common base")}
			currentDiffs := slices.Clone(selectedDiffs)
			selectedHistory := []v1.History{oldBase}
			currentHistory := []v1.History{currentBase}
			if changedTail {
				selectedDiffs = append(selectedDiffs, digest.FromString("old physical COPY"))
				currentDiffs = append(currentDiffs, digest.FromString("new physical COPY"))
				selectedHistory = append(selectedHistory, v1.History{Created: &oldTime, CreatedBy: "COPY payload /payload"})
				currentHistory = append(currentHistory, v1.History{Created: &currentTime, CreatedBy: "COPY payload /payload"})
			}
			parentLayers := len(selectedDiffs)
			selectedDiffs = append(selectedDiffs, digest.FromString("selected complete RUN"))
			selectedHistory = append(selectedHistory, v1.History{CreatedBy: "RUN compile"})
			builder.OCIv1 = v1.Image{RootFS: v1.RootFS{Type: "layers", DiffIDs: slices.Clone(selectedDiffs)}, History: selectedHistory}
			builder.Docker.History = dockerHistory(selectedHistory)
			changed, err := replaceGraphCachedMetadata(builder, cachedMetadataReplacement{ParentDiffIDs: currentDiffs, OCIBase: currentHistory, DockerBase: dockerHistory(currentHistory)}, "RUN compile", parentLayers)
			if err != nil || !changed {
				t.Fatalf("history restore changed=%v error=%v", changed, err)
			}
			if !equalOCIHistory(builder.OCIv1.History[0], currentBase) {
				t.Fatalf("current base history lost: %+v", builder.OCIv1.History[0])
			}
			if changedTail && !builder.OCIv1.History[1].Created.Equal(oldTime) {
				t.Fatal("changed physical COPY lost its selected provenance")
			}
			if !slices.Equal(builder.OCIv1.RootFS.DiffIDs, selectedDiffs) {
				t.Fatal("restoration grafted physical layers")
			}
		})
	}
}

func TestGraphRelayedParentPinsReadyImageDespiteNewerAlternative(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	copyKey := digest.FromString("logical COPY")
	var relayed *instructionVertex
	for _, physical := range []string{"ready A", "newer B"} {
		engine, err := newInstructionSolver(ctx, solverTestStore{root: root}, nil)
		if err != nil {
			t.Fatal(err)
		}
		source := engine.source(digest.FromString("scratch"), instructionSolverResult{}, false)
		pending, err := engine.begin(ctx, source, copyKey, "COPY", physical == "newer B")
		if err != nil {
			t.Fatal(err)
		}
		result := &instructionSolverResult{Entry: instructionCacheEntry{ImageID: physical, ManifestDigest: digest.FromString(physical)}}
		if err := pending.finish(result); err != nil {
			t.Fatal(err)
		}
		if physical == "ready A" {
			complete, err := engine.results.read(result.ID())
			if err != nil {
				t.Fatal(err)
			}
			relayed = &instructionVertex{key: copyKey, dgst: pending.vertex.Digest(), parent: source, source: complete}
		}
		if err := engine.close(); err != nil {
			t.Fatal(err)
		}
	}
	engine, err := newInstructionSolver(ctx, solverTestStore{root: root}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := engine.close(); err != nil {
			t.Error(err)
		}
	}()
	readyBuilder := &upstream.Builder{FromImageID: "ready A", OCIv1: v1.Image{Platform: v1.Platform{OS: "linux", Architecture: "amd64"}, RootFS: v1.RootFS{Type: "layers", DiffIDs: []digest.Digest{digest.FromString("ready A filesystem")}}}}
	pinned, err := engine.pinCompletedGraphParent(readyBuilder, nil, readyBuilder.OCIv1.Platform, relayed, false)
	if err != nil {
		t.Fatal(err)
	}
	if pinned.parent != nil || pinned.source.Entry.ImageID != "ready A" {
		t.Fatal("completed stage did not become the ready immutable boundary")
	}
	pending, err := engine.begin(ctx, pinned, digest.FromString("new RUN"), "RUN", false)
	if err != nil {
		t.Fatal(err)
	}
	if pending.request == nil || pending.request.parent.Entry.ImageID != "ready A" {
		t.Fatal("new consumer selected another producer than the prepared ready image")
	}
	pending.abort()
	// A forced ancestor must remain authoritative after collapsing the relay.
	relayed.parent.ignore = true
	forced, err := engine.pinCompletedGraphParent(readyBuilder, nil, readyBuilder.OCIv1.Platform, relayed, false)
	if err != nil || !forced.ignore {
		t.Fatalf("forced ancestor lost at completed boundary: %v", err)
	}
}
