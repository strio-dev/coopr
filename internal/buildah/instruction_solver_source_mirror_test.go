package buildah

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/moby/buildkit/solver"
	digest "github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/coopr/internal/cache"
)

func refreshedInstructionSourceRecord(t *testing.T, original cache.GraphRecord, added time.Duration) cache.GraphRecord {
	t.Helper()
	raw, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	var record cache.GraphRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		t.Fatal(err)
	}
	var metadata instructionGraphMetadata
	if err := json.Unmarshal(record.Metadata, &metadata); err != nil {
		t.Fatal(err)
	}
	for i := range record.Results {
		var value instructionSolverResult
		if err := json.Unmarshal(record.Results[i].Metadata, &value); err != nil {
			t.Fatal(err)
		}
		value.CreatedAt = value.CreatedAt.Add(added)
		record.Results[i].Metadata, err = json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
	}
	for i := range metadata.Nodes {
		for j := range metadata.Nodes[i].Results {
			metadata.Nodes[i].Results[j].CreatedAt = metadata.Nodes[i].Results[j].CreatedAt.Add(added)
		}
	}
	record.Metadata, err = json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	record.CreatedAt = record.CreatedAt.Add(added)
	return record
}

func instructionMirrorFixture(t *testing.T, badNewer bool, damage string, reversed ...bool) (*instructionSolver, cache.GraphRecord, string) {
	t.Helper()
	first, firstPath, original, manifests := instructionSourceFixture(t)
	record, path, err := first.LookupGraph(t.Context(), original.Root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(path) }()
	secondPath := t.TempDir()
	second, err := cache.NewLocalStore(t.Context(), secondPath, t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	refreshed := refreshedInstructionSourceRecord(t, *record, time.Hour)
	badRecord, goodRecord := *record, refreshed
	if badNewer {
		badRecord, goodRecord = refreshed, *record
	}
	if _, err := first.PutGraph(t.Context(), badRecord.Root, badRecord, path); err != nil {
		t.Fatal(err)
	}
	if _, err := second.PutGraph(t.Context(), goodRecord.Root, goodRecord, path); err != nil {
		t.Fatal(err)
	}
	// Both metadata snapshots remain valid while exact image payload availability
	// differs. Metadata admission must preserve both approved mirror handles.
	imagePath := filepath.Join(firstPath, "blobs", "sha256", record.Results[1].Image.Digest.Encoded())
	if damage == "missing" {
		if err := os.Remove(imagePath); err != nil {
			t.Fatal(err)
		}
	} else {
		// Corrupt a payload rather than an image root: ORAS checks every
		// indexed root when opening a layout, before graph metadata admission.
		var manifest v1.Manifest
		if err := json.Unmarshal(manifests[record.Results[1].Image.Digest.String()], &manifest); err != nil {
			t.Fatal(err)
		}
		imagePath = filepath.Join(firstPath, "blobs", "sha256", manifest.Config.Digest.Encoded())
		if err := os.Chmod(imagePath, 0600); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(imagePath)
		if err != nil {
			t.Fatal(err)
		}
		data[0] ^= 1
		if err := os.WriteFile(imagePath, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	firstBinding := instructionReadBindingID(CacheSpec{Transport: "oci-layout", Reference: firstPath})
	secondBinding := instructionReadBindingID(CacheSpec{Transport: "oci-layout", Reference: secondPath})
	engine := instructionSourceEngine(t, first, firstBinding, manifests)
	engine.portable.cache.readStores = append(engine.portable.cache.readStores, second)
	engine.portable.cache.readBindingIDs = append(engine.portable.cache.readBindingIDs, secondBinding)
	if len(reversed) != 0 && reversed[0] {
		slices.Reverse(engine.portable.cache.readStores)
		slices.Reverse(engine.portable.cache.readBindingIDs)
	}
	if err := engine.preparePortable(t.Context(), engine.source(record.Root, instructionSolverResult{}, false)); err != nil {
		t.Fatal(err)
	}
	return engine, refreshed, secondPath
}

func TestInstructionGraphEquivalentMirrorsSelectedAndExport(t *testing.T) {
	for _, badNewer := range []bool{false, true} {
		for _, damage := range []string{"missing", "corrupt"} {
			for _, action := range []string{"selected", "export"} {
				for _, reversed := range []bool{false, true} {
					name := map[bool]string{false: "bad-older", true: "bad-newer"}[badNewer] + "/" + damage + "/" + action + "/" + map[bool]string{false: "bad-first", true: "good-first"}[reversed]
					t.Run(name, func(t *testing.T) {
						engine, record, goodPath := instructionMirrorFixture(t, badNewer, damage, reversed)
						var goodSource *cache.GraphSource
						for _, source := range engine.portable.sources {
							if source.binding == instructionReadBindingID(CacheSpec{Transport: "oci-layout", Reference: goodPath}) {
								goodSource = source.source
							}
						}
						selected := record.Results[1]
						pending := engine.results.lazy.get(selected.ID)
						var imports atomic.Int32
						pending.importImage = func(ctx context.Context) (string, error) {
							if _, chosen, err := pending.ensureLayout(ctx); err != nil {
								return "", err
							} else if chosen == nil || chosen != goodSource {
								t.Fatal("import selected source differs from successful fallback")
							}
							imports.Add(1)
							return selected.Image.Digest.String(), nil
						}
						if action == "selected" {
							if _, err := engine.results.Load(t.Context(), solver.CacheResult{ID: selected.ID}); err != nil {
								t.Fatal("valid exact mirror was lost", err)
							}
							native, err := engine.results.read(selected.ID)
							if err != nil {
								t.Fatal(err)
							}
							if !native.CreatedAt.Equal(pending.value.CreatedAt) || imports.Load() != 1 {
								t.Fatal("fallback changed selected timestamp or native import count")
							}
						} else {
							export, err := newInstructionGraphExport(t.TempDir())
							if err != nil {
								t.Fatal(err)
							}
							defer func() { _ = os.RemoveAll(export.dir) }()
							for _, result := range record.Results {
								got, err := export.result(t.Context(), engine.portable, result.ID)
								if err != nil {
									t.Fatal("exact mirror failed complete export", err)
								}
								var value instructionSolverResult
								if err := json.Unmarshal(got.Metadata, &value); err != nil {
									t.Fatal(err)
								}
								if value.ID() != result.ID || !value.CreatedAt.Equal(engine.results.lazy.get(result.ID).value.CreatedAt) {
									t.Fatal("mirror downgraded canonical admitted result metadata")
								}
							}
							if imports.Load() != 0 {
								t.Fatal("export imported native historical images")
							}
						}
						if pending.failed.Load() {
							t.Fatal("failed mirror poisoned exact result across valid sources")
						}
						if len(pending.locations.locations) != 2 {
							t.Fatal("equivalent source handles were discarded")
						}
						var value instructionSolverResult
						if err := json.Unmarshal(record.Results[1].Metadata, &value); err != nil {
							t.Fatal(err)
						}
						if !pending.value.CreatedAt.Equal(value.CreatedAt) {
							t.Fatal("source fallback changed admitted timestamp authority")
						}
					})
				}
			}
		}
	}
}

func TestInstructionGraphEquivalentMirrorsPreserveWorkerBindings(t *testing.T) {
	child, record, _ := instructionMirrorFixture(t, true, "corrupt")
	bindings := child.portable.cache.readBindingIDs
	relays, err := child.relayInstructionSources(bindings)
	if err != nil {
		t.Fatal(err)
	}
	if len(relays) != 2 || relays[0].Binding == relays[1].Binding {
		t.Fatal("worker lost independent approved bindings")
	}
	parent := instructionSourceEngine(t, child.portable.cache.readStores[0].(*cache.OCIStore), bindings[0], child.results.store.(instructionSourceStore).manifests)
	parent.portable.cache.readStores = child.portable.cache.readStores
	parent.portable.cache.readBindingIDs = bindings
	if err := child.closePortable(); err != nil {
		t.Fatal(err)
	}
	if err := parent.acceptInstructionSources(t.Context(), relays); err != nil {
		t.Fatal(err)
	}
	export, err := newInstructionGraphExport(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(export.dir) }()
	for _, result := range record.Results {
		if _, err := export.result(t.Context(), parent.portable, result.ID); err != nil {
			t.Fatal("parent lost valid exact mirror after child exit", err)
		}
	}
}

func TestInstructionGraphEquivalentMirrorsAllUnavailableAndCanceled(t *testing.T) {
	for _, action := range []string{"selected", "export"} {
		t.Run(action, func(t *testing.T) {
			engine, record, secondPath := instructionMirrorFixture(t, true, "missing")
			// Remove only the exact selected image from the second fixture-owned
			// cache. Both admitted metadata envelopes remain available and unchanged.
			if err := os.Remove(filepath.Join(secondPath, "blobs", "sha256", record.Results[1].Image.Digest.Encoded())); err != nil {
				t.Fatal(err)
			}
			candidate := engine.results.lazy.get(record.Results[1].ID)
			canceled, cancel := context.WithCancel(t.Context())
			cancel()
			if _, _, err := candidate.ensureLayout(canceled); err != context.Canceled {
				t.Fatal("mirror selection lost cancellation", err)
			}
			var err error
			if action == "selected" {
				candidate.importImage = func(ctx context.Context) (string, error) { _, _, err := candidate.ensureLayout(ctx); return "", err }
				_, err = engine.results.Load(t.Context(), solver.CacheResult{ID: candidate.value.ID()})
			} else {
				export, e := newInstructionGraphExport(t.TempDir())
				if e != nil {
					t.Fatal(e)
				}
				defer func() { _ = os.RemoveAll(export.dir) }()
				_, err = export.result(t.Context(), engine.portable, candidate.value.ID())
			}
			if err == nil || !strings.Contains(err.Error(), "no available approved source") || !strings.Contains(err.Error(), "approved graph source 1") || !strings.Contains(err.Error(), "approved graph source 2") {
				t.Fatal("all-source failure lost exact candidate diagnostics", err)
			}
		})
	}
}

func TestInstructionGraphEquivalentMirrorsRejectDescriptorConflict(t *testing.T) {
	engine, record, _ := instructionMirrorFixture(t, true, "missing")
	previous := engine.results.lazy.get(record.Results[1].ID)
	conflict := &instructionPortableResult{value: previous.value, record: previous.record, source: previous.source}
	conflict.record.Image.Size++
	if err := engine.results.lazy.add([]*instructionPortableResult{conflict}, nil); err == nil {
		t.Fatal("same ID admitted another descriptor identity")
	}
	if engine.results.lazy.get(previous.value.ID()) != previous {
		t.Fatal("conflicting source replaced approved result")
	}
}

func TestInstructionGraphWorkerUnavailableParentBindingRetainsCompleteBarrier(t *testing.T) {
	for _, approved := range []bool{false, true} {
		t.Run(map[bool]string{false: "new-child-only-store", true: "parent-approved-store"}[approved], func(t *testing.T) {
			store, path, record, manifests := instructionSourceFixture(t)
			binding := instructionReadBindingID(CacheSpec{Transport: "oci-layout", Reference: path})
			if err := os.Remove(filepath.Join(path, "blobs", "sha256", record.Results[1].Image.Digest.Encoded())); err != nil {
				t.Fatal(err)
			}
			engine := instructionSourceEngine(t, store, binding, manifests)
			engine.portable.relayBindingsSet = true
			if approved {
				engine.portable.relayBindings = []digest.Digest{binding}
			}
			if err := engine.preparePortable(t.Context(), engine.source(record.Root, instructionSolverResult{}, false)); err != nil {
				t.Fatal(err)
			}
			if approved {
				if len(engine.portable.sources) != 1 || engine.results.lazy.get(record.Results[0].ID) == nil {
					t.Fatal("approved source lost selective metadata admission")
				}
			} else {
				if engine.portable.cache.stats.Errors != 1 || len(engine.portable.sources) != 0 || engine.results.lazy.get(record.Results[0].ID) != nil {
					t.Fatal("child-only binding escaped complete validation/import barrier")
				}
			}
		})
	}
}

func TestInstructionGraphEquivalentMirrorsNewLocationRevivesFailedResult(t *testing.T) {
	engine, record, _ := instructionMirrorFixture(t, true, "missing")
	previous := engine.results.lazy.get(record.Results[1].ID)
	previous.failed.Store(true) // Earlier approved locations were exhausted.
	good := engine.portable.cache.readStores[1].(*cache.OCIStore)
	older, layout, err := good.LookupGraph(t.Context(), record.Root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(layout) }()
	path := t.TempDir()
	next, err := cache.NewLocalStore(t.Context(), path, t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := next.PutGraph(t.Context(), older.Root, *older, layout); err != nil {
		t.Fatal(err)
	}
	engine.portable.cache.readStores = append(engine.portable.cache.readStores, next)
	engine.portable.cache.readBindingIDs = append(engine.portable.cache.readBindingIDs, instructionReadBindingID(CacheSpec{Transport: "oci-layout", Reference: path}))
	source, err := engine.portable.reader.LookupMetadata(t.Context(), next, record.Root)
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.portable.restoreSource(t.Context(), source); err != nil {
		t.Fatal(err)
	}
	pending := engine.results.lazy.get(previous.value.ID())
	if pending == previous || pending.failed.Load() || !pending.value.CreatedAt.Equal(previous.value.CreatedAt) {
		t.Fatal("new exact source failed to revive result while retaining timestamp")
	}
	pending.importImage = func(ctx context.Context) (string, error) {
		if _, _, err := pending.ensureLayout(ctx); err != nil {
			return "", err
		}
		return pending.record.Image.Digest.String(), nil
	}
	if _, err := engine.results.Load(t.Context(), solver.CacheResult{ID: pending.value.ID()}); err != nil {
		t.Fatal(err)
	}
	count := len(pending.locations.locations)
	if err := engine.portable.restoreSource(t.Context(), source); err != nil {
		t.Fatal(err)
	}
	if len(pending.locations.locations) != count {
		t.Fatal("duplicate admitted mirror retained another location")
	}
}
