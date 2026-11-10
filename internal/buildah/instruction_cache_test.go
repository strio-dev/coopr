package buildah

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/coopr/internal/cache"
	"github.com/strio-dev/coopr/internal/definition"
	"github.com/strio-dev/coopr/internal/imageconfig"
	"github.com/strio-dev/coopr/internal/planner"
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

func instructionCacheTestInput(operation planner.Operation) instructionCacheInput {
	return instructionCacheInput{
		ParentRootFS: "parent-one",
		Logical:      imageconfig.New(),
		Operation:    operation,
		Platform:     v1.Platform{OS: "linux", Architecture: "amd64"},
		Isolation:    "rootless",
		Runtime:      "crun",
		Format:       "oci",
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
		{"parent", func(value *instructionCacheInput) { value.ParentRootFS = "parent-two" }},
		{"platform", func(value *instructionCacheInput) { value.Platform.Architecture = "arm64" }},
		{"OS version", func(value *instructionCacheInput) { value.Platform.OSVersion = "new" }},
		{"OS features", func(value *instructionCacheInput) { value.Platform.OSFeatures = []string{"required"} }},
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
		{"hostname", func(value *instructionCacheInput) {
			var err error
			value.Logical, err = imageconfig.Parse([]byte(`{"config":{"Env":["MODE=one"],"Hostname":"explicit-host"}}`))
			if err != nil {
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

func TestInstructionCacheKeyTracksExecutionConfig(t *testing.T) {
	operation := planner.Operation{
		Instruction: definition.Instruction{Name: "run", Form: "shell", Arguments: []string{"printf ready >/proof"}},
	}
	base := instructionCacheTestInput(operation)
	baseKey, cacheable, err := instructionCacheKey(base)
	if err != nil || !cacheable {
		t.Fatalf("base instructionCacheKey() = %q, %v, %v", baseKey, cacheable, err)
	}

	for _, change := range []struct {
		instruction definition.Instruction
		wantSame    bool
	}{
		{definition.Instruction{Name: "label", Arguments: []string{"org.example.release", "two"}}, true},
		{definition.Instruction{Name: "cmd", Form: "exec", Arguments: []string{"/bin/echo", "two"}}, true},
		{definition.Instruction{Name: "env", Arguments: []string{"MODE", "two"}}, false},
		{definition.Instruction{Name: "user", Arguments: []string{"1234:1234"}}, false},
		{definition.Instruction{Name: "workdir", Arguments: []string{"/workspace"}}, false},
	} {
		t.Run(change.instruction.Name, func(t *testing.T) {
			changed := base
			changed.Logical = base.Logical.Clone()
			if err := changed.Logical.Apply(change.instruction); err != nil {
				t.Fatal(err)
			}
			got, cacheable, err := instructionCacheKey(changed)
			if err != nil || !cacheable {
				t.Fatalf("changed instructionCacheKey() = %q, %v, %v", got, cacheable, err)
			}
			if same := got == baseKey; same != change.wantSame {
				t.Fatalf("%s: RUN cache keys %s and %s, want equal=%v", change.instruction.Name, baseKey, got, change.wantSame)
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

func TestInstructionCacheRetentionPreservesNames(t *testing.T) {
	store := newInstructionCacheTestStore(t)
	old := createInstructionCacheTestImage(t, store, "example.test/user-name:latest")
	fresh := createInstructionCacheTestImage(t, store, "example.test/fresh-name:latest")
	first, second := digest.FromString("first"), digest.FromString("second")
	for _, item := range []struct {
		image string
		key   digest.Digest
	}{{old, first}, {fresh, first}, {fresh, second}} {
		if err := retainInstructionCacheImage(store, item.image, item.key); err != nil {
			t.Fatal(err)
		}
	}
	for _, key := range []digest.Digest{first, second} {
		image, err := store.Image(instructionCacheName(key))
		if err != nil || image.ID != fresh {
			t.Fatalf("retention key %s: image=%+v, err=%v", key, image, err)
		}
	}
	for _, item := range []struct{ image, name string }{{old, "example.test/user-name:latest"}, {fresh, "example.test/fresh-name:latest"}} {
		image, err := store.Image(item.image)
		if err != nil || !slices.Contains(image.Names, item.name) {
			t.Fatalf("unrelated name lost: image=%+v, err=%v", image, err)
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

type unavailableInstructionCacheStore struct{ err error }

func (s unavailableInstructionCacheStore) LookupImage(context.Context, cache.ImageKey) (*cache.ImageRecord, string, error) {
	return nil, "", s.err
}
func (s unavailableInstructionCacheStore) PutImage(context.Context, cache.ImageKey, cache.ImageRecord, string) (v1.Descriptor, error) {
	return v1.Descriptor{}, s.err
}
func TestInstructionCacheCandidateFailureWarnsWithCause(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent", "staging")
	c := &portableInstructionCache{stagingDir: path, writeStores: []instructionCacheStore{unavailableInstructionCacheStore{}}}
	warning := packageWarningOutput(t, func() {
		c.recordImage(context.Background(), nil, cache.ImageKey{}, "", "", nil, nil, nil, "", nil, nil)
	})
	if c.stats.Errors != 1 || len(c.candidates) != 0 || !strings.Contains(warning, "stage instruction cache candidate") || !strings.Contains(warning, "no such file") {
		t.Fatalf("candidate failure hidden: warning=%q stats=%+v", warning, c.stats)
	}
}

func TestRunMountCacheKeyUsesVerifiedFilesystemInsteadOfStorageHandle(t *testing.T) {
	for _, kind := range []string{"image-rootfs", "artifact-files"} {
		t.Run(kind, func(t *testing.T) {
			makeInput := func(selector string) instructionCacheInput {
				mount := definition.Instruction{Name: "mount", Arguments: []string{"bind"}, Properties: map[string]string{"from": selector, "source": "/selected", "target": "/input", "readonly": "true"}}
				input := instructionCacheTestInput(planner.Operation{Instruction: definition.Instruction{Name: "run", Arguments: []string{"cat /input/payload"}, Children: []definition.Instruction{mount}}})
				input.ResolvedInputsComplete = true
				input.ResolvedInputs = []instructionCacheResolvedInput{{Kind: kind, Identity: digest.FromString("verified-filesystem").String()}}
				return input
			}
			key := func(input instructionCacheInput) digest.Digest {
				t.Helper()
				key, cacheable, err := instructionCacheKey(input)
				if err != nil || !cacheable {
					t.Fatalf("key = %s, %t, %v", key, cacheable, err)
				}
				return key
			}
			first := makeInput("containers-storage:first")
			firstKey := key(first)
			if second := key(makeInput("containers-storage:second")); second != firstKey {
				t.Fatalf("equivalent verified filesystem missed cache: %s != %s", firstKey, second)
			}
			if first.Operation.Children[0].Properties["from"] != "containers-storage:first" {
				t.Fatal("key construction mutated authored operation")
			}
			for name, change := range map[string]func(*instructionCacheInput){
				"filesystem": func(v *instructionCacheInput) {
					v.ResolvedInputs[0].Identity = digest.FromString("changed-filesystem").String()
				},
				"path":     func(v *instructionCacheInput) { v.Operation.Children[0].Properties["source"] = "/other" },
				"readonly": func(v *instructionCacheInput) { v.Operation.Children[0].Properties["readonly"] = "false" },
				"target":   func(v *instructionCacheInput) { v.Operation.Children[0].Properties["target"] = "/elsewhere" },
			} {
				t.Run(name, func(t *testing.T) {
					changed := makeInput("containers-storage:first")
					change(&changed)
					if key(changed) == firstKey {
						t.Fatal("relevant mount input did not invalidate cache")
					}
				})
			}
			incomplete := makeInput("containers-storage:first")
			incomplete.ResolvedInputsComplete = false
			if _, cacheable, err := instructionCacheKey(incomplete); err != nil || cacheable {
				t.Fatalf("incomplete mount identity cacheable: %t %v", cacheable, err)
			}
			invalid := makeInput("containers-storage:first")
			invalid.ResolvedInputs[0].Identity = "unverified"
			otherInvalid := makeInput("containers-storage:second")
			otherInvalid.ResolvedInputs[0].Identity = "unverified"
			if key(invalid) == key(otherInvalid) {
				t.Fatal("unverified identity normalized a storage handle")
			}
		})
	}
}

func TestCopyAddCacheKeyUsesMeasuredInputInsteadOfStorageHandle(t *testing.T) {
	for _, operation := range []string{"copy", "add"} {
		t.Run(operation, func(t *testing.T) {
			makeInput := func(selector string) instructionCacheInput {
				input := instructionCacheTestInput(planner.Operation{Instruction: definition.Instruction{
					Name: operation, Arguments: []string{"/source", "/destination"},
					Properties: map[string]string{"from": selector},
				}})
				input.InputDigest = digest.FromString("same-measured-file-input")
				return input
			}
			key := func(input instructionCacheInput) digest.Digest {
				t.Helper()
				key, cacheable, err := instructionCacheKey(input)
				if err != nil || !cacheable {
					t.Fatalf("key = %s, %t, %v", key, cacheable, err)
				}
				return key
			}
			first := makeInput("containers-storage:first")
			original := first.Operation.Properties["from"]
			firstKey := key(first)
			if second := key(makeInput("containers-storage:second")); second != firstKey {
				t.Fatalf("equivalent measured COPY input missed cache: %s != %s", firstKey, second)
			}
			if first.Operation.Properties["from"] != original {
				t.Fatal("key construction mutated the authored source selector")
			}
			for name, change := range map[string]func(*instructionCacheInput){
				"contents and metadata": func(v *instructionCacheInput) { v.InputDigest = digest.FromString("changed-measured-input") },
				"selected path":         func(v *instructionCacheInput) { v.Operation.Arguments[0] = "/other" },
				"destination":           func(v *instructionCacheInput) { v.Operation.Arguments[1] = "/other" },
				"ownership":             func(v *instructionCacheInput) { v.Operation.Properties["chown"] = "1:2" },
				"mode":                  func(v *instructionCacheInput) { v.Operation.Properties["chmod"] = "0755" },
				"platform":              func(v *instructionCacheInput) { v.Platform.Architecture = "arm64" },
				"caller config": func(v *instructionCacheInput) {
					v.Logical = v.Logical.Clone()
					if err := v.Logical.Apply(definition.Instruction{Name: "workdir", Arguments: []string{"/changed"}}); err != nil {
						t.Fatal(err)
					}
				},
			} {
				t.Run(name, func(t *testing.T) {
					changed := makeInput("containers-storage:first")
					change(&changed)
					if key(changed) == firstKey {
						t.Fatal("relevant COPY/ADD input did not invalidate cache")
					}
				})
			}
			unmeasured := makeInput("containers-storage:first")
			unmeasured.InputDigest = ""
			if _, cacheable, err := instructionCacheKey(unmeasured); err != nil || cacheable {
				t.Fatalf("unmeasured input cacheable: %t %v", cacheable, err)
			}
		})
	}
}
