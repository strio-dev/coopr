package buildah

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/moby/buildkit/solver"
	digest "github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/coopr/internal/cache"
	"oras.land/oras-go/v2/content"
)

type instructionGraphPublicationTarget struct {
	*cache.OCIStore
	calls []digest.Digest
	fail  error
}

func (target *instructionGraphPublicationTarget) PutGraph(ctx context.Context, root digest.Digest, record cache.GraphRecord, layout string) (v1.Descriptor, error) {
	target.calls = append(target.calls, root)
	if target.fail != nil {
		return v1.Descriptor{}, target.fail
	}
	return target.OCIStore.PutGraph(ctx, root, record, layout)
}

func instructionGraphPublicationFixture(t *testing.T) (*instructionSolver, *instructionGraphPublicationTarget, cache.GraphRecord, string, map[string][]byte) {
	t.Helper()
	store, sourcePath, record, manifests := instructionSourceFixture(t)
	binding := instructionReadBindingID(CacheSpec{Transport: "oci-layout", Reference: sourcePath})
	engine := instructionSourceEngine(t, store, binding, manifests)
	if err := engine.preparePortable(t.Context(), engine.source(record.Root, instructionSolverResult{}, false)); err != nil {
		t.Fatal(err)
	}
	destination, err := cache.NewLocalStore(t.Context(), t.TempDir(), t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	target := &instructionGraphPublicationTarget{OCIStore: destination}
	engine.portable.cache.writeStores = []instructionCacheStore{target}
	return engine, target, record, sourcePath, manifests
}

func addInstructionGraphPublicationRoot(t *testing.T, engine *instructionSolver, key digest.Digest, results ...cache.GraphResult) {
	t.Helper()
	node := instructionGraphNode{ID: solver.NewCacheKey(key, "", 0).ID}
	pending := make([]*instructionPortableResult, 0, len(results))
	for _, result := range results {
		value := engine.results.lazy.get(result.ID)
		pending = append(pending, value)
		node.Results = append(node.Results, solver.CacheResult{ID: result.ID, CreatedAt: value.value.CreatedAt})
	}
	if err := engine.results.lazy.add(pending, []instructionGraphNode{node}); err != nil {
		t.Fatal(err)
	}
	engine.portable.roots[key] = instructionGraphRoot{vertex: engine.source(key, instructionSolverResult{}, false), result: results[0].ID}
}

func instructionGraphPublicationPayload(t *testing.T, source string, result cache.GraphResult, manifests map[string][]byte) string {
	t.Helper()
	var manifest v1.Manifest
	if err := json.Unmarshal(manifests[result.Image.Digest.String()], &manifest); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(source, "blobs", "sha256", manifest.Config.Digest.Encoded())
}

func TestInstructionGraphPublicationSkipsWholeRootWithInvalidImportedBytes(t *testing.T) {
	for _, damage := range []string{"digest", "truncated", "trailing"} {
		for _, badFirst := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/bad-first-%t", damage, badFirst), func(t *testing.T) {
				engine, target, record, source, manifests := instructionGraphPublicationFixture(t)
				keys := []digest.Digest{digest.FromString("publication one"), digest.FromString("publication two")}
				slices.Sort(keys)
				badRoot, goodRoot := keys[0], keys[1]
				if !badFirst {
					badRoot, goodRoot = goodRoot, badRoot
				}
				addInstructionGraphPublicationRoot(t, engine, badRoot, record.Results[1], record.Results[0])
				addInstructionGraphPublicationRoot(t, engine, goodRoot, record.Results[0])
				// A failed read must neither overwrite this complete artifact nor
				// replace its healthy alternatives with a partial fresh graph.
				original := record
				original.Root = badRoot
				_, originalLayout, err := engine.portable.cache.readStores[0].(*cache.OCIStore).LookupGraph(t.Context(), record.Root)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.RemoveAll(originalLayout) })
				before, err := target.OCIStore.PutGraph(t.Context(), badRoot, original, originalLayout)
				if err != nil {
					t.Fatal(err)
				}
				payload := instructionGraphPublicationPayload(t, source, record.Results[1], manifests)
				data, err := os.ReadFile(payload)
				if err != nil {
					t.Fatal(err)
				}
				switch damage {
				case "digest":
					data[0] ^= 1
				case "truncated":
					data = data[:len(data)-1]
				case "trailing":
					data = append(data, 'x')
				}
				if err := os.Chmod(payload, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(payload, data, 0600); err != nil {
					t.Fatal(err)
				}
				export, err := newInstructionGraphExport(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.RemoveAll(export.dir) })
				_, exportErr := export.result(t.Context(), engine.portable, record.Results[1].ID)
				var invalid *instructionGraphInputIntegrityError
				if !errors.As(exportErr, &invalid) {
					t.Fatalf("direct complete export accepted invalid bytes: %v", exportErr)
				}
				warning := packageWarningOutput(t, func() {
					if err := engine.publishPortable(t.Context()); err != nil {
						t.Fatal("invalid optional input failed the completed build", err)
					}
				})
				if !strings.Contains(warning, "skip graph cache publication") || !strings.Contains(warning, badRoot.String()) {
					t.Fatal("skipped graph was not reported", warning)
				}
				if !slices.Equal(target.calls, []digest.Digest{goodRoot}) || engine.portable.cache.stats.Stored != 1 || engine.portable.cache.stats.Errors != 1 {
					t.Fatalf("partial or skipped graph reported as stored: calls=%v stats=%+v", target.calls, engine.portable.cache.stats)
				}
				reader, err := cache.NewGraphReader(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = reader.Close() })
				retained, err := reader.LookupMetadata(t.Context(), target.OCIStore, badRoot)
				if err != nil || retained.Artifact().Digest != before.Digest {
					t.Fatal("failed input replaced the previous complete graph", err)
				}
				good, layout, err := target.LookupGraph(t.Context(), goodRoot)
				if err != nil {
					t.Fatal("independent healthy graph was not published completely", err)
				}
				t.Cleanup(func() { _ = os.RemoveAll(layout) })
				if len(good.Results) != 1 || good.Results[0].ID != record.Results[0].ID {
					t.Fatalf("healthy graph changed its result: %+v", good.Results)
				}
			})
		}
	}
}

func TestInstructionGraphInputIntegrityRejectsMixedAndUnprovenFailures(t *testing.T) {
	for _, cause := range []error{content.ErrMismatchedDigest, content.ErrTrailingData, io.ErrUnexpectedEOF} {
		if !instructionGraphInputIntegrityOnly(fmt.Errorf("source: %w", errors.Join(cause, cause))) {
			t.Fatal("proven invalid input was not classified", cause)
		}
	}
	for _, cause := range []error{
		nil,
		context.Canceled,
		syscall.ENOSPC,
		os.ErrPermission,
		os.ErrNotExist,
		io.ErrClosedPipe,
		errors.New(content.ErrMismatchedDigest.Error()),
		errors.Join(content.ErrMismatchedDigest, io.ErrClosedPipe),
		errors.Join(content.ErrMismatchedDigest, context.Canceled),
		cache.ErrGraphSourceClose,
		errors.Join(cache.ErrGraphSourceClose, io.ErrUnexpectedEOF),
		errors.Join(cache.ErrGraphSourceClose, content.ErrMismatchedDigest),
		errors.Join(content.ErrMismatchedDigest, fmt.Errorf("close: %w", errors.Join(cache.ErrGraphSourceClose, io.ErrUnexpectedEOF))),
	} {
		if instructionGraphInputIntegrityOnly(cause) {
			t.Fatal("unproven or mixed failure was softened", cause)
		}
	}
}

func TestInstructionGraphPublicationKeepsOtherFailuresFatal(t *testing.T) {
	for _, failure := range []string{"missing retained input", "canceled", "staging IO", "native export", "write"} {
		t.Run(failure, func(t *testing.T) {
			engine, target, record, source, manifests := instructionGraphPublicationFixture(t)
			root := digest.FromString("fatal publication")
			addInstructionGraphPublicationRoot(t, engine, root, record.Results[0])
			ctx := t.Context()
			var want error
			switch failure {
			case "missing retained input":
				if err := os.Remove(instructionGraphPublicationPayload(t, source, record.Results[0], manifests)); err != nil {
					t.Fatal(err)
				}
			case "canceled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
				want = context.Canceled
			case "staging IO":
				path := filepath.Join(t.TempDir(), "file")
				if err := os.WriteFile(path, []byte("not a staging directory"), 0600); err != nil {
					t.Fatal(err)
				}
				engine.portable.cache.stagingDir = path
				want = syscall.ENOTDIR
			case "native export":
				value := engine.results.lazy.get(record.Results[0].ID).value
				// The fake native store accepts this opaque ID, but the real
				// storage transport rejects its digest prefix during export.
				value.Entry.ImageID = record.Results[0].Image.Digest.String()
				if _, err := engine.results.Save(&value, value.CreatedAt); err != nil {
					t.Fatal(err)
				}
			case "write":
				target.fail, want = syscall.ENOSPC, syscall.ENOSPC
			}
			err := engine.publishPortable(ctx)
			var invalid *instructionGraphInputIntegrityError
			if err == nil || errors.As(err, &invalid) || want != nil && !errors.Is(err, want) {
				t.Fatalf("non-input failure changed classification: %v, want %v", err, want)
			}
			if failure == "native export" && !strings.Contains(err.Error(), "create cached storage reference") {
				t.Fatalf("did not exercise the native export path: %v", err)
			}
			if failure == "write" && !slices.Equal(target.calls, []digest.Digest{root}) {
				t.Fatalf("did not exercise the write failure: %v", target.calls)
			}
			if failure != "write" && len(target.calls) != 0 || engine.portable.cache.stats.Stored != 0 {
				t.Fatalf("failed staging published a graph: calls=%v stats=%+v", target.calls, engine.portable.cache.stats)
			}
		})
	}
}
