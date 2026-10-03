package buildah

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"coopr/internal/cache"
	"coopr/internal/definition"
	"coopr/internal/imageconfig"
	"coopr/internal/planner"
	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"go.podman.io/storage"
)

type failingInstructionCacheStore struct {
	err error
}

func (s failingInstructionCacheStore) LookupImage(context.Context, cache.ImageKey) (*cache.ImageRecord, string, error) {
	return nil, "", cache.ErrMiss
}

func (s failingInstructionCacheStore) PutImage(context.Context, cache.ImageKey, cache.ImageRecord, string) (v1.Descriptor, error) {
	return v1.Descriptor{}, s.err
}

func TestInstructionCacheExportReturnsAllFailuresAfterCleanup(t *testing.T) {
	root := t.TempDir()
	layout := filepath.Join(root, "candidate", "layout")
	if err := os.MkdirAll(layout, 0o700); err != nil {
		t.Fatal(err)
	}
	c := &portableInstructionCache{
		writeStores: []instructionCacheStore{
			failingInstructionCacheStore{err: errors.New("first export failed")},
			failingInstructionCacheStore{err: errors.New("second export failed")},
		},
		candidates: []portableInstructionCandidate{{layout: layout}},
	}
	err := c.publish(context.Background())
	if err == nil || !strings.Contains(err.Error(), "first export failed") || !strings.Contains(err.Error(), "second export failed") {
		t.Fatalf("instruction cache export error = %v", err)
	}
	if len(c.candidates) != 0 || c.stats.Errors != 2 || c.stats.Stored != 0 {
		t.Fatalf("instruction cache after failed export = candidates %d stats %+v", len(c.candidates), c.stats)
	}
	if _, err := os.Stat(filepath.Dir(layout)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("staged instruction cache remains: %v", err)
	}
}

func TestPortableInstructionCacheRelaysDeferredCandidate(t *testing.T) {
	workerRoot := t.TempDir()
	parentRoot := t.TempDir()
	layout := filepath.Join(workerRoot, "candidate", "layout")
	if err := os.MkdirAll(layout, 0o700); err != nil {
		t.Fatal(err)
	}
	manifest := v1.Descriptor{MediaType: v1.MediaTypeImageManifest, Digest: digest.FromString("manifest"), Size: 123}
	index, err := json.Marshal(v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, Manifests: []v1.Descriptor{manifest}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(layout, "index.json"), index, 0o600); err != nil {
		t.Fatal(err)
	}
	key := cache.ImageKey{
		Instruction: digest.FromString("instruction"), Parent: digest.FromString("parent"),
		Platform: v1.Platform{OS: "linux", Architecture: "amd64"}, Executor: "executor", Format: "oci",
	}
	worker := &portableInstructionCache{stagingDir: workerRoot, candidates: []portableInstructionCandidate{{
		key: key, record: cache.ImageRecord{Version: cache.ImageRecordVersion, Key: key, Image: manifest}, layout: layout,
	}}}
	relays, err := worker.relayCandidates(parentRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.close(); err != nil {
		t.Fatal(err)
	}
	parent := &portableInstructionCache{stagingDir: parentRoot}
	if err := parent.acceptRelayedCandidates(relays); err != nil {
		t.Fatal(err)
	}
	if len(parent.candidates) != 1 {
		t.Fatalf("accepted %d candidates, want 1", len(parent.candidates))
	}
	if _, err := os.Stat(filepath.Join(parent.candidates[0].layout, "index.json")); err != nil {
		t.Fatalf("relayed layout did not survive worker cleanup: %v", err)
	}
}

func instructionCacheTestInput(operation planner.Operation) instructionCacheInput {
	return instructionCacheInput{
		ParentImageID: "parent-one",
		Logical:       imageconfig.New(),
		Operation:     operation,
		Platform:      v1.Platform{OS: "linux", Architecture: "amd64"},
		Isolation:     "rootless",
		Runtime:       "crun",
		Format:        "oci",
	}
}

func TestInstructionCacheKeyCapturesRunInputs(t *testing.T) {
	config := imageconfig.New()
	if err := config.Apply(definition.Instruction{Name: "env", Arguments: []string{"MODE", "one"}}); err != nil {
		t.Fatal(err)
	}
	operation := planner.Operation{
		Instruction: definition.Instruction{
			Name: "run", Form: "shell", Arguments: []string{"printf ready >/proof"},
		},
		ArgumentsInScope: map[string]string{"target": "one"},
	}
	input := instructionCacheTestInput(operation)
	input.Logical = config

	key, ok, err := instructionCacheKey(input)
	if err != nil || !ok || key == "" {
		t.Fatalf("instructionCacheKey() = %q, %v, %v", key, ok, err)
	}

	changes := []struct {
		name   string
		mutate func(*instructionCacheInput)
	}{
		{"parent", func(value *instructionCacheInput) { value.ParentImageID = "parent-two" }},
		{"platform", func(value *instructionCacheInput) { value.Platform.Architecture = "arm64" }},
		{"isolation", func(value *instructionCacheInput) { value.Isolation = "chroot" }},
		{"runtime", func(value *instructionCacheInput) { value.Runtime = "runc" }},
		{"format", func(value *instructionCacheInput) { value.Format = "docker" }},
		{"add host", func(value *instructionCacheInput) { value.AddHosts = []string{"example.test:127.0.0.1"} }},
		{"run controls", func(value *instructionCacheInput) { value.RunControls.Memory = 64 * 1024 * 1024 }},
		{"compat volumes", func(value *instructionCacheInput) { value.CompatVolumes = true }},
		{"network", func(value *instructionCacheInput) {
			value.Operation.Properties = map[string]string{"network": "none"}
		}},
		{"command", func(value *instructionCacheInput) { value.Operation.Arguments = []string{"printf changed >/proof"} }},
		{"argument", func(value *instructionCacheInput) {
			value.Operation.ArgumentsInScope = map[string]string{"target": "two"}
		}},
		{"config", func(value *instructionCacheInput) {
			value.Logical = value.Logical.Clone()
			if err := value.Logical.Apply(definition.Instruction{Name: "env", Arguments: []string{"MODE", "two"}}); err != nil {
				t.Fatal(err)
			}
		}},
		{"source date epoch", func(value *instructionCacheInput) { epoch := int64(123); value.SourceDateEpoch = &epoch }},
		{"rewrite timestamp", func(value *instructionCacheInput) { value.RewriteTimestamp = true }},
		{"root metadata", func(value *instructionCacheInput) {
			value.RootMetadata = &PackageRootMetadata{Mode: 0o711, UID: 1, GID: 2, PAXRecords: map[string]string{"SCHILY.xattr.user.coopr": "value"}}
		}},
	}
	for _, change := range changes {
		t.Run(change.name, func(t *testing.T) {
			changed := input
			change.mutate(&changed)
			got, cacheable, err := instructionCacheKey(changed)
			if err != nil || !cacheable {
				t.Fatalf("changed instructionCacheKey() = %q, %v, %v", got, cacheable, err)
			}
			if got == key {
				t.Fatalf("cache key did not change for %s", change.name)
			}
		})
	}
}

func TestPortableInstructionDigestUsesOCIParent(t *testing.T) {
	input := instructionCacheTestInput(planner.Operation{
		Instruction: definition.Instruction{Name: "run", Form: "shell", Arguments: []string{"printf ready >/proof"}, Properties: map[string]string{"network": "none"}},
	})
	parent := digest.FromString("selected parent manifest")
	first, cacheable, err := portableInstructionDigest(input, parent)
	if err != nil || !cacheable {
		t.Fatalf("portableInstructionDigest() = %q, %v, %v", first, cacheable, err)
	}
	input.ParentImageID = "a different private store image ID"
	second, cacheable, err := portableInstructionDigest(input, parent)
	if err != nil || !cacheable || second != first {
		t.Fatalf("private image ID changed portable digest: first=%s second=%s cacheable=%v err=%v", first, second, cacheable, err)
	}
	changedParent, cacheable, err := portableInstructionDigest(input, digest.FromString("other selected parent manifest"))
	if err != nil || !cacheable || changedParent == first {
		t.Fatalf("OCI parent did not change portable digest: first=%s changed=%s cacheable=%v err=%v", first, changedParent, cacheable, err)
	}
}

func TestPortableInstructionParentUsesDomainSeparatedScratchIdentity(t *testing.T) {
	if got := portableInstructionParent(""); got != portableInstructionScratchParent || got.Validate() != nil {
		t.Fatalf("scratch parent = %q, want valid %q", got, portableInstructionScratchParent)
	}
	manifest := digest.FromString("manifest")
	if got := portableInstructionParent(manifest); got != manifest {
		t.Fatalf("manifest parent = %q, want %q", got, manifest)
	}
}

func TestPortableInstructionEligibilityUsesDeclaredInputs(t *testing.T) {
	localAdd := Add{Sources: []string{"archive.tar"}}
	tests := []struct {
		name    string
		planned planner.Operation
		lowered Operation
		want    bool
	}{
		{name: "isolated run", planned: cacheTestOperationWithProperties("run", map[string]string{"network": "none"}), lowered: Run{}, want: true},
		{name: "default network", planned: cacheTestOperation("run", "true"), lowered: Run{}, want: true},
		{name: "run mount", planned: cacheTestOperationWithProperties("run", map[string]string{"network": "none"}), lowered: Run{Mounts: []RunMount{{Type: "cache"}}}, want: true},
		{name: "run device", planned: cacheTestOperationWithProperties("run", map[string]string{"network": "none"}), lowered: Run{Devices: []runDeviceRequest{{Name: "vendor.com/device=all"}}}},
		{name: "local copy", planned: planner.Operation{Instruction: definition.Instruction{Name: "copy", Arguments: []string{"src", "/dst"}}, InputContext: "build"}, lowered: Copy{}, want: true},
		{name: "child input", planned: planner.Operation{Instruction: definition.Instruction{Name: "copy", Children: []definition.Instruction{{Name: "source"}}}}, lowered: Copy{}, want: true},
		{name: "stage source", planned: planner.Operation{Instruction: definition.Instruction{Name: "copy", Properties: map[string]string{"from": "producer"}}}, lowered: copyFromImageOperation{}, want: true},
		{name: "named context", planned: planner.Operation{Instruction: definition.Instruction{Name: "copy", Properties: map[string]string{"from": "assets"}}, InputContext: "assets"}, lowered: copyFromImageOperation{}, want: true},
		{name: "local add", planned: cacheTestOperation("add", "archive.tar", "/dst"), lowered: localAdd, want: true},
		{name: "remote add", planned: cacheTestOperation("add", "https://example.invalid/archive.tar", "/dst"), lowered: Add{Sources: []string{"https://example.invalid/archive.tar"}}},
		{name: "pinned remote add", planned: cacheTestOperation("add", "https://example.invalid/archive.tar", "/dst"), lowered: Add{Sources: []string{"https://example.invalid/archive.tar"}, Checksum: digest.FromString("archive").String()}, want: true},
		{name: "remote git add", planned: cacheTestOperation("add", "https://example.invalid/repository.git#main", "/dst"), lowered: Add{Sources: []string{"https://example.invalid/repository.git#main"}}},
		{name: "pinned remote git add", planned: cacheTestOperation("add", "https://example.invalid/repository.git#main", "/dst"), lowered: Add{Sources: []string{"https://example.invalid/repository.git#main"}, Checksum: "0123456789ab"}, want: true},
		{name: "workdir", planned: cacheTestOperation("workdir", "/workspace"), lowered: WorkDir("/workspace"), want: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := portableInstructionEligible(test.planned, test.lowered, RunControls{}); got != test.want {
				t.Fatalf("portableInstructionEligible() = %v, want %v", got, test.want)
			}
			_, runs := test.lowered.(Run)
			wantWithDevice := test.want && !runs
			if got := portableInstructionEligible(test.planned, test.lowered, RunControls{Devices: []string{"/dev/test-device"}}); got != wantWithDevice {
				t.Fatalf("portableInstructionEligible(global device) = %v, want %v", got, wantWithDevice)
			}
		})
	}
}

func cacheTestOperationWithProperties(name string, properties map[string]string) planner.Operation {
	return planner.Operation{Instruction: definition.Instruction{Name: name, Properties: properties}}
}

func TestInstructionCacheKeyIgnoresOutputOnlyConfig(t *testing.T) {
	operation := planner.Operation{
		Instruction: definition.Instruction{Name: "run", Form: "shell", Arguments: []string{"printf ready >/proof"}},
	}
	base := instructionCacheTestInput(operation)
	baseKey, cacheable, err := instructionCacheKey(base)
	if err != nil || !cacheable {
		t.Fatalf("base instructionCacheKey() = %q, %v, %v", baseKey, cacheable, err)
	}

	changes := []definition.Instruction{
		{Name: "label", Arguments: []string{"org.example.release", "two"}},
		{Name: "cmd", Form: "exec", Arguments: []string{"/bin/echo", "two"}},
	}
	for _, change := range changes {
		t.Run(change.Name, func(t *testing.T) {
			changed := base
			changed.Logical = base.Logical.Clone()
			if err := changed.Logical.Apply(change); err != nil {
				t.Fatal(err)
			}
			got, cacheable, err := instructionCacheKey(changed)
			if err != nil || !cacheable {
				t.Fatalf("changed instructionCacheKey() = %q, %v, %v", got, cacheable, err)
			}
			if got != baseKey {
				t.Fatalf("output-only %s changed RUN cache key: got %s, want %s", change.Name, got, baseKey)
			}
		})
	}
}

func TestInstructionCacheKeyCapturesExecutionConfig(t *testing.T) {
	operation := planner.Operation{
		Instruction: definition.Instruction{Name: "run", Form: "shell", Arguments: []string{"printf ready >/proof"}},
	}
	base := instructionCacheTestInput(operation)
	baseKey, cacheable, err := instructionCacheKey(base)
	if err != nil || !cacheable {
		t.Fatalf("base instructionCacheKey() = %q, %v, %v", baseKey, cacheable, err)
	}

	changes := []definition.Instruction{
		{Name: "env", Arguments: []string{"MODE", "two"}},
		{Name: "user", Arguments: []string{"1234:1234"}},
		{Name: "workdir", Arguments: []string{"/workspace"}},
	}
	for _, change := range changes {
		t.Run(change.Name, func(t *testing.T) {
			changed := base
			changed.Logical = base.Logical.Clone()
			if err := changed.Logical.Apply(change); err != nil {
				t.Fatal(err)
			}
			got, cacheable, err := instructionCacheKey(changed)
			if err != nil || !cacheable {
				t.Fatalf("changed instructionCacheKey() = %q, %v, %v", got, cacheable, err)
			}
			if got == baseKey {
				t.Fatalf("execution-relevant %s did not change RUN cache key %s", change.Name, baseKey)
			}
		})
	}
}

func TestInstructionCacheKeySupportsOrderedFilesystemOperations(t *testing.T) {
	mount := definition.Instruction{Name: "mount", Arguments: []string{"bind"}, Properties: map[string]string{"target": "/src"}}
	tests := []struct {
		name  string
		input instructionCacheInput
	}{
		{"default-network run", instructionCacheTestInput(planner.Operation{Instruction: definition.Instruction{Name: "run", Arguments: []string{"true"}}})},
		{"explicit-network run", instructionCacheTestInput(planner.Operation{Instruction: definition.Instruction{Name: "run", Arguments: []string{"true"}, Properties: map[string]string{"network": "default"}}})},
		{"workdir", instructionCacheTestInput(planner.Operation{Instruction: definition.Instruction{Name: "workdir", Arguments: []string{"/workspace"}}})},
		{"copy", func() instructionCacheInput {
			input := instructionCacheTestInput(planner.Operation{Instruction: definition.Instruction{Name: "copy", Arguments: []string{"file", "/file"}}})
			input.InputDigest = digest.FromString("copy stream")
			return input
		}()},
		{"add", func() instructionCacheInput {
			input := instructionCacheTestInput(planner.Operation{Instruction: definition.Instruction{Name: "add", Arguments: []string{"archive.tar", "/"}}})
			input.InputDigest = digest.FromString("add stream")
			return input
		}()},
		{"run with resolved mount", func() instructionCacheInput {
			input := instructionCacheTestInput(planner.Operation{Instruction: definition.Instruction{Name: "run", Arguments: []string{"cat /src/file"}, Children: []definition.Instruction{mount}}})
			input.ResolvedInputsComplete = true
			input.ResolvedInputs = []instructionCacheResolvedInput{{Kind: "context", Identity: digest.FromString("context input").String()}}
			return input
		}()},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			key, cacheable, err := instructionCacheKey(test.input)
			if err != nil || !cacheable || key == "" {
				t.Fatalf("instructionCacheKey() = %q, %v, %v", key, cacheable, err)
			}
		})
	}
}

func TestInstructionCacheKeyCapturesInputDigestAndResolvedInputs(t *testing.T) {
	copyInput := instructionCacheTestInput(planner.Operation{Instruction: definition.Instruction{Name: "copy", Arguments: []string{"file", "/file"}}})
	copyInput.InputDigest = digest.FromString("first stream")
	first, cacheable, err := instructionCacheKey(copyInput)
	if err != nil || !cacheable {
		t.Fatalf("first COPY cache key = %q, %v, %v", first, cacheable, err)
	}
	copyInput.InputDigest = digest.FromString("second stream")
	second, cacheable, err := instructionCacheKey(copyInput)
	if err != nil || !cacheable {
		t.Fatalf("second COPY cache key = %q, %v, %v", second, cacheable, err)
	}
	if first == second {
		t.Fatal("input stream digest did not change the COPY cache key")
	}

	mount := definition.Instruction{Name: "mount", Arguments: []string{"bind"}, Properties: map[string]string{"target": "/src"}}
	runInput := instructionCacheTestInput(planner.Operation{Instruction: definition.Instruction{Name: "run", Arguments: []string{"cat /src/file"}, Children: []definition.Instruction{mount}}})
	runInput.ResolvedInputsComplete = true
	runInput.ResolvedInputs = []instructionCacheResolvedInput{{Kind: "stage", Identity: "image-one"}}
	first, cacheable, err = instructionCacheKey(runInput)
	if err != nil || !cacheable {
		t.Fatalf("first mounted RUN cache key = %q, %v, %v", first, cacheable, err)
	}
	runInput.ResolvedInputs[0].Identity = "image-two"
	second, cacheable, err = instructionCacheKey(runInput)
	if err != nil || !cacheable {
		t.Fatalf("second mounted RUN cache key = %q, %v, %v", second, cacheable, err)
	}
	if first == second {
		t.Fatal("resolved mount identity did not change the RUN cache key")
	}
}

func TestInstructionCacheKeyNormalizesDefaultOCIFormat(t *testing.T) {
	input := instructionCacheTestInput(planner.Operation{Instruction: definition.Instruction{Name: "run", Arguments: []string{"true"}}})
	input.Format = ""
	implicit, cacheable, err := instructionCacheKey(input)
	if err != nil || !cacheable {
		t.Fatalf("implicit OCI key = %q, %v, %v", implicit, cacheable, err)
	}
	input.Format = "oci"
	explicit, cacheable, err := instructionCacheKey(input)
	if err != nil || !cacheable {
		t.Fatalf("explicit OCI key = %q, %v, %v", explicit, cacheable, err)
	}
	if implicit != explicit {
		t.Fatalf("implicit OCI key %s differs from explicit OCI key %s", implicit, explicit)
	}
	input.Format = "unknown"
	if _, _, err := instructionCacheKey(input); err == nil {
		t.Fatal("unknown format was accepted")
	}
}

func TestInstructionCacheKeyRejectsIncompleteInputs(t *testing.T) {
	mount := definition.Instruction{Name: "mount", Arguments: []string{"bind"}, Properties: map[string]string{"target": "/src"}}
	base := instructionCacheTestInput(planner.Operation{Instruction: definition.Instruction{Name: "run", Arguments: []string{"true"}}})
	tests := []struct {
		name   string
		mutate func(*instructionCacheInput)
	}{
		{"missing isolation", func(value *instructionCacheInput) { value.Isolation = "" }},
		{"missing platform", func(value *instructionCacheInput) { value.Platform.Architecture = "" }},
		{"missing runtime", func(value *instructionCacheInput) { value.Runtime = "" }},
		{"unsupported operation", func(value *instructionCacheInput) { value.Operation.Name = "env" }},
		{"copy without input digest", func(value *instructionCacheInput) { value.Operation.Name = "copy" }},
		{"add without input digest", func(value *instructionCacheInput) { value.Operation.Name = "add" }},
		{"invalid input digest", func(value *instructionCacheInput) {
			value.Operation.Name = "copy"
			value.InputDigest = "invalid"
		}},
		{"mount identities incomplete", func(value *instructionCacheInput) { value.Operation.Children = []definition.Instruction{mount} }},
		{"mount identity count mismatch", func(value *instructionCacheInput) {
			value.Operation.Children = []definition.Instruction{mount}
			value.ResolvedInputsComplete = true
		}},
		{"empty mount identity", func(value *instructionCacheInput) {
			value.Operation.Children = []definition.Instruction{mount}
			value.ResolvedInputsComplete = true
			value.ResolvedInputs = []instructionCacheResolvedInput{{Kind: "context"}}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := base
			test.mutate(&input)
			key, cacheable, err := instructionCacheKey(input)
			if err != nil {
				t.Fatal(err)
			}
			if cacheable || key != "" {
				t.Fatalf("instructionCacheKey() = %q, %v; want cache bypass", key, cacheable)
			}
		})
	}
}

func TestInstructionCacheKeyRequiresLogicalConfig(t *testing.T) {
	input := instructionCacheTestInput(planner.Operation{Instruction: definition.Instruction{Name: "workdir", Arguments: []string{"/workspace"}}})
	input.Logical = nil
	if _, _, err := instructionCacheKey(input); err == nil {
		t.Fatal("nil logical config was accepted")
	}
}

func TestInstructionCacheIndexLookupAndStaleRecord(t *testing.T) {
	store := newInstructionCacheTestStore(t)
	key := digest.FromString("wanted instruction")
	staleKey := digest.FromString("stale instruction")
	stale := createInstructionCacheTestImage(t, store, "example.test/user-name:latest")

	staleData, err := json.Marshal(instructionCacheRecord{Schema: instructionCacheSchema, Key: staleKey})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetImageBigData(stale, instructionCacheBigDataName(key), staleData, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.AddNames(stale, []string{instructionCacheName(key)}); err != nil {
		t.Fatal(err)
	}
	if got, err := findInstructionCache(store, key); err != nil || got != "" {
		t.Fatalf("stale findInstructionCache() = %q, %v; want miss", got, err)
	}

	fresh := createInstructionCacheTestImage(t, store, "example.test/fresh-name:latest")
	if err := storeInstructionCache(store, fresh, key); err != nil {
		t.Fatal(err)
	}
	if got, err := findInstructionCache(store, key); err != nil || got != fresh {
		t.Fatalf("findInstructionCache() = %q, %v; want %q", got, err, fresh)
	}
	entry, err := findInstructionCacheEntry(store, key)
	if err != nil {
		t.Fatal(err)
	}
	wantManifest := digest.FromBytes([]byte(`{"schemaVersion":2}`))
	if entry.ManifestDigest != wantManifest {
		t.Fatalf("cache manifest digest = %s, want %s", entry.ManifestDigest, wantManifest)
	}

	staleImage, err := store.Image(stale)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(staleImage.Names, "example.test/user-name:latest") {
		t.Fatalf("stale image names = %v; unrelated name was removed", staleImage.Names)
	}
	if slices.Contains(staleImage.Names, instructionCacheName(key)) {
		t.Fatalf("stale image names = %v; cache name was not moved", staleImage.Names)
	}
	freshImage, err := store.Image(fresh)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(freshImage.Names, "example.test/fresh-name:latest") {
		t.Fatalf("fresh image names = %v; unrelated name was removed", freshImage.Names)
	}
}

func TestInstructionCacheRecordsCommittedManifestAfterDefaultChanges(t *testing.T) {
	store := newInstructionCacheTestStore(t)
	imageID := createInstructionCacheTestImage(t, store)
	committed := digest.FromBytes([]byte(`{"schemaVersion":2}`))
	other := []byte(`{"schemaVersion":2,"mediaType":"other"}`)
	if err := store.SetImageBigData(imageID, storage.ImageDigestBigDataKey, other, func(data []byte) (digest.Digest, error) {
		return digest.FromBytes(data), nil
	}); err != nil {
		t.Fatal(err)
	}
	key := digest.FromString("exact committed manifest")
	if err := storeInstructionCacheEntrySelected(store, imageID, committed, key, nil); err != nil {
		t.Fatal(err)
	}
	entry, err := findInstructionCacheEntry(store, key)
	if err != nil {
		t.Fatal(err)
	}
	if entry.ManifestDigest != committed {
		t.Fatalf("cached manifest = %s, want committed %s", entry.ManifestDigest, committed)
	}
}

func TestInstructionCacheTTLUsesRecordCreationTime(t *testing.T) {
	store := newInstructionCacheTestStore(t)
	imageID := createInstructionCacheTestImage(t, store)
	key := digest.FromString("ttl instruction")
	manifest := digest.FromBytes([]byte(`{"schemaVersion":2}`))
	if err := storeInstructionCacheEntrySelectedAt(store, imageID, manifest, key, nil, time.Now().Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	ttl := time.Hour
	entry, err := findInstructionCacheEntry(store, key, &ttl)
	if err != nil {
		t.Fatal(err)
	}
	if entry.ImageID != "" {
		t.Fatalf("expired instruction cache returned %s", entry.ImageID)
	}
	entry, err = findInstructionCacheEntry(store, key)
	if err != nil || entry.ImageID != imageID {
		t.Fatalf("cache without TTL = %+v, %v", entry, err)
	}
}

func TestInstructionCacheIndexTreatsMissingNameAndMetadataAsMiss(t *testing.T) {
	store := newInstructionCacheTestStore(t)
	key := digest.FromString("missing instruction")
	if got, err := findInstructionCache(store, key); err != nil || got != "" {
		t.Fatalf("missing findInstructionCache() = %q, %v; want miss", got, err)
	}

	imageID := createInstructionCacheTestImage(t, store, instructionCacheName(key))
	if got, err := findInstructionCache(store, key); err != nil || got != "" {
		t.Fatalf("metadata-less findInstructionCache() = %q, %v; want miss", got, err)
	}
	if image, err := store.Image(imageID); err != nil || image.ID != imageID {
		t.Fatalf("metadata-less image lookup = %+v, %v", image, err)
	}
}

func TestInstructionCacheIndexConcurrentStores(t *testing.T) {
	store := newInstructionCacheTestStore(t)
	key := digest.FromString("concurrent instruction")
	const workers = 8
	imageIDs := make([]string, workers)
	for index := range imageIDs {
		imageIDs[index] = createInstructionCacheTestImage(t, store)
	}

	var wait sync.WaitGroup
	errorsByWorker := make([]error, workers)
	for index, imageID := range imageIDs {
		wait.Add(1)
		go func() {
			defer wait.Done()
			errorsByWorker[index] = storeInstructionCache(store, imageID, key)
		}()
	}
	wait.Wait()
	for index, err := range errorsByWorker {
		if err != nil {
			t.Fatalf("worker %d: %v", index, err)
		}
	}
	got, err := findInstructionCache(store, key)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(imageIDs, got) {
		t.Fatalf("findInstructionCache() = %q; want one of %v", got, imageIDs)
	}
}

func TestInstructionCacheIndexKeepsMultipleKeysOnOneImage(t *testing.T) {
	store := newInstructionCacheTestStore(t)
	imageID := createInstructionCacheTestImage(t, store)
	first := digest.FromString("first instruction on shared image")
	second := digest.FromString("second instruction on shared image")

	if err := storeInstructionCache(store, imageID, first); err != nil {
		t.Fatal(err)
	}
	if err := storeInstructionCache(store, imageID, second); err != nil {
		t.Fatal(err)
	}
	for _, key := range []digest.Digest{first, second} {
		got, err := findInstructionCache(store, key)
		if err != nil || got != imageID {
			t.Fatalf("findInstructionCache(%s) = %q, %v; want %q", key, got, err, imageID)
		}
	}
}

func newInstructionCacheTestStore(t *testing.T) storage.Store {
	t.Helper()
	root := t.TempDir()
	store, err := storage.GetStore(storage.StoreOptions{
		RunRoot: root + "/run", GraphRoot: root + "/graph", GraphDriverName: "vfs",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := store.Shutdown(false); err != nil {
			t.Errorf("shut down instruction cache test store: %v", err)
		}
	})
	return store
}

func createInstructionCacheTestImage(t *testing.T, store storage.Store, names ...string) string {
	t.Helper()
	image, err := store.CreateImage("", names, "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetImageBigData(image.ID, storage.ImageDigestBigDataKey, []byte(`{"schemaVersion":2}`), func(data []byte) (digest.Digest, error) { return digest.FromBytes(data), nil }); err != nil {
		t.Fatal(err)
	}
	return image.ID
}
