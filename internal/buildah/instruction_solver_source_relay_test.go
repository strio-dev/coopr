package buildah

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	digest "github.com/opencontainers/go-digest"
)

func TestInstructionGraphSourceFileValidation(t *testing.T) {
	relay := []instructionGraphSourceRelay{{Binding: 0, Root: digest.FromString("root")}}
	for _, test := range []struct {
		name   string
		change func(*testing.T, string, *instructionGraphSourceFile)
	}{
		{"size", func(_ *testing.T, _ string, s *instructionGraphSourceFile) { s.Size++ }},
		{"digest", func(_ *testing.T, _ string, s *instructionGraphSourceFile) { s.Digest = digest.FromString("changed") }},
		{"symlink", func(t *testing.T, p string, _ *instructionGraphSourceFile) {
			if err := os.Rename(p, p+".target"); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(p+".target", p); err != nil {
				t.Fatal(err)
			}
		}},
		{"hardlink", func(t *testing.T, p string, _ *instructionGraphSourceFile) {
			if err := os.Link(p, p+".link"); err != nil {
				t.Fatal(err)
			}
		}},
		{"public mode", func(t *testing.T, p string, _ *instructionGraphSourceFile) {
			if err := os.Chmod(p, 0644); err != nil {
				t.Fatal(err)
			}
		}},
		{"directory", func(t *testing.T, p string, _ *instructionGraphSourceFile) {
			if err := os.Remove(p); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(p, 0700); err != nil {
				t.Fatal(err)
			}
		}},
		{"trailing", func(t *testing.T, p string, s *instructionGraphSourceFile) {
			data, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			data = append(data, []byte(" []")...)
			if err := os.WriteFile(p, data, 0600); err != nil {
				t.Fatal(err)
			}
			s.Digest = digest.FromBytes(data)
			s.Size = int64(len(data))
		}},
		{"unknown field", func(t *testing.T, p string, s *instructionGraphSourceFile) {
			data := []byte(`[{"binding":0,"root":"` + digest.FromString("root").String() + `","unexpected":true}]`)
			if err := os.WriteFile(p, data, 0600); err != nil {
				t.Fatal(err)
			}
			s.Digest = digest.FromBytes(data)
			s.Size = int64(len(data))
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "result.json")
			summary, err := writeInstructionGraphSources(path, relay)
			if err != nil {
				t.Fatal(err)
			}
			test.change(t, filepath.Join(filepath.Dir(path), instructionGraphSourcesName), summary)
			if _, err := readInstructionGraphSources(path, summary); err == nil {
				t.Fatal("accepted unsafe graph source file")
			}
		})
	}
	path := filepath.Join(t.TempDir(), "result.json")
	summary, err := writeInstructionGraphSources(path, relay)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writeInstructionGraphSources(path, relay); err == nil {
		t.Fatal("overwrote existing relay file")
	}
	decoded, err := readInstructionGraphSources(path, summary)
	if err != nil || len(decoded) != 1 || decoded[0].Root != relay[0].Root {
		t.Fatal("valid relay file rejected", err)
	}
	if _, err := readInstructionGraphSources(path, &instructionGraphSourceFile{Digest: digest.FromString("huge"), Size: workerMaxJSON + 1}); err == nil {
		t.Fatal("accepted oversized relay")
	}
}

func TestInstructionGraphSourcesRequireParentBinding(t *testing.T) {
	store, cachePath, record, manifests := instructionSourceFixture(t)
	binding := instructionReadBindingID(CacheSpec{Transport: "oci-layout", Reference: cachePath})
	child := instructionSourceEngine(t, store, binding, manifests)
	if err := child.preparePortable(t.Context(), child.source(record.Root, instructionSolverResult{}, false)); err != nil {
		t.Fatal(err)
	}
	if _, err := child.relayInstructionSources([]digest.Digest{digest.FromString("unapproved")}); err == nil {
		t.Fatal("child escaped parent binding set")
	}
	relays, err := child.relayInstructionSources([]digest.Digest{binding})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(relays)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		change func(*instructionGraphSourceRelay)
	}{
		{"negative binding", func(r *instructionGraphSourceRelay) { r.Binding = -1 }},
		{"unknown binding", func(r *instructionGraphSourceRelay) { r.Binding = 1 }},
		{"different root", func(r *instructionGraphSourceRelay) { r.Root = digest.FromString("different") }},
		{"changed metadata", func(r *instructionGraphSourceRelay) { r.Metadata.Record = append(r.Metadata.Record, ' ') }},
	} {
		t.Run(test.name, func(t *testing.T) {
			var relays []instructionGraphSourceRelay
			if err := json.Unmarshal(raw, &relays); err != nil {
				t.Fatal(err)
			}
			test.change(&relays[0])
			parent := instructionSourceEngine(t, store, binding, manifests)
			if err := parent.acceptInstructionSources(t.Context(), relays); err == nil {
				t.Fatal("accepted invalid source relay")
			}
			if len(parent.portable.sources) != 0 {
				t.Fatal("invalid source was retained")
			}
		})
	}
}
