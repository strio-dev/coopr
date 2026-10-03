package buildah

import (
	"reflect"
	"strings"
	"testing"

	"coopr/internal/definition"
	"github.com/opencontainers/go-digest"
	upstream "go.podman.io/buildah"
)

func TestUniqueSimpleCopyCacheCandidate(t *testing.T) {
	first := simpleCopyCacheCandidate{key: digest.FromString("first"), input: digest.FromString("input-first")}
	second := simpleCopyCacheCandidate{key: digest.FromString("second"), input: digest.FromString("input-second")}
	tests := []struct {
		name       string
		candidates []simpleCopyCacheCandidate
		want       *simpleCopyCacheCandidate
		ambiguous  bool
	}{
		{name: "miss"},
		{name: "unique", candidates: []simpleCopyCacheCandidate{first}, want: &first},
		{name: "ambiguous", candidates: []simpleCopyCacheCandidate{first, second}, ambiguous: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, ambiguous := uniqueSimpleCopyCacheCandidate(test.candidates)
			if !reflect.DeepEqual(got, test.want) || ambiguous != test.ambiguous {
				t.Fatalf("uniqueSimpleCopyCacheCandidate() = %#v, %v; want %#v, %v", got, ambiguous, test.want, test.ambiguous)
			}
		})
	}
}

func TestCopyAddProbeEligibility(t *testing.T) {
	inline := []definition.InlineFile{{Path: "inline", Data: "contents"}}
	tests := []struct {
		name      string
		operation Operation
		want      bool
	}{
		{name: "copy", operation: Copy{Sources: []string{"file"}}, want: true},
		{name: "inline copy", operation: Copy{InlineFiles: inline}, want: true},
		{name: "local add", operation: Add{Sources: []string{"archive.tar"}}, want: true},
		{name: "inline add", operation: Add{InlineFiles: inline}, want: true},
		{name: "remote add", operation: Add{Sources: []string{"https://example.invalid/archive.tar"}}},
		{name: "git add", operation: Add{Sources: []string{"https://example.invalid/repository.git#main"}}},
		{name: "SSH git add", operation: Add{Sources: []string{"git@example.invalid:repository.git#main"}}},
		{name: "linked copy", operation: Copy{Sources: []string{"file"}, Link: true}},
		{name: "linked add", operation: Add{Sources: []string{"file"}, Link: true}},
		{name: "image copy", operation: copyFromImageOperation{}, want: true},
		{name: "linked image copy", operation: copyFromImageOperation{options: upstream.AddAndCopyOptions{Link: true}}},
		{name: "other", operation: Env{Name: "A", Value: "B"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := copyAddProbeEligible(test.operation); got != test.want {
				t.Fatalf("copyAddProbeEligible(%T) = %v, want %v", test.operation, got, test.want)
			}
		})
	}
}

func TestPinnedHTTPAddInputDigest(t *testing.T) {
	checksum := digest.FromString("remote payload")
	got, declared, err := pinnedHTTPAddInputDigest(Add{
		Sources: []string{"https://example.invalid/payload"}, Checksum: checksum.String(),
	})
	if err != nil || !declared || got != checksum {
		t.Fatalf("pinnedHTTPAddInputDigest() = %q, %v, %v; want %q, true, nil", got, declared, err, checksum)
	}

	for _, operation := range []Operation{
		Add{Sources: []string{"https://example.invalid/payload"}},
		Add{Sources: []string{"https://example.invalid/repository.git#main"}, Checksum: "0123456789ab"},
		Add{Sources: []string{"payload"}, Checksum: checksum.String()},
	} {
		if got, declared, err := pinnedHTTPAddInputDigest(operation); err != nil || declared || got != "" {
			t.Fatalf("unpinned input digest = %q, %v, %v; want empty, false, nil", got, declared, err)
		}
	}

	if _, _, err := pinnedHTTPAddInputDigest(Add{
		Sources: []string{"https://example.invalid/payload"}, Checksum: "invalid",
	}); err == nil || !strings.Contains(err.Error(), "invalid ADD checksum") {
		t.Fatalf("invalid checksum error = %v", err)
	}
}

func TestCopyAddDigestUsesDryRunOnlyForProbe(t *testing.T) {
	contextDir := t.TempDir()
	operation := Copy{
		Sources: []string{"source"}, Destination: "/destination", Chown: "12:34", Chmod: "0640",
		Parents: true, Excludes: []string{"*.tmp"},
	}
	builder := &recordingBuilder{}
	if _, err := probeCopyAddDigest(builder, contextDir, nil, operation); err != nil {
		t.Fatal(err)
	}
	if _, err := applyCopyAddWithDigest(builder, contextDir, nil, operation); err != nil {
		t.Fatal(err)
	}
	if len(builder.addOptions) != 2 {
		t.Fatalf("Buildah Add calls = %d, want 2", len(builder.addOptions))
	}
	probe, applied := builder.addOptions[0], builder.addOptions[1]
	if !probe.DryRun || applied.DryRun {
		t.Fatalf("Buildah DryRun options = %v, %v, want true, false", probe.DryRun, applied.DryRun)
	}
	if probe.Hasher == nil || applied.Hasher == nil {
		t.Fatal("COPY digest path omitted Buildah Hasher")
	}
	wantExcludes := []string{"*.tmp", "source/*.tmp"}
	if probe.ContextDir != contextDir || applied.ContextDir != contextDir || probe.Chown != operation.Chown || probe.Chmod != operation.Chmod || !probe.Parents || !reflect.DeepEqual(probe.Excludes, wantExcludes) {
		t.Fatalf("probe options = %#v, applied options = %#v", probe, applied)
	}
	if !probe.StripSetuidBit || !probe.StripSetgidBit || !applied.StripSetuidBit || !applied.StripSetgidBit {
		t.Fatalf("COPY digest paths omitted set-ID stripping: probe=%#v applied=%#v", probe, applied)
	}
}

func TestApplyCopyAddWithDigestValidatesBeforeExecution(t *testing.T) {
	tests := []struct {
		name      string
		context   string
		artifacts []string
		operation Operation
		want      string
	}{
		{name: "empty copy", operation: Copy{}, want: "COPY requires at least one source and a destination"},
		{name: "empty add", operation: Add{}, want: "ADD requires at least one source and a destination"},
		{
			name: "remote copy", context: t.TempDir(),
			operation: Copy{Sources: []string{"https://example.invalid/file"}, Destination: "/file"},
			want:      `COPY source must be local: "https://example.invalid/file"`,
		},
		{
			name: "artifacts without context", artifacts: []string{"output"},
			operation: Copy{Sources: []string{"file"}, Destination: "/file"},
			want:      "context artifacts require a build context",
		},
		{name: "unsupported operation", operation: Env{Name: "A", Value: "B"}, want: "is not COPY or ADD"},
		{
			name: "copy from validates store", operation: copyFromImageOperation{
				imageID: "source", sources: []string{"/file"}, destination: "/file",
			},
			want: "COPY --from store is nil",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			digested, err := applyCopyAddWithDigest(nativeBuilder{}, test.context, test.artifacts, test.operation)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("applyCopyAddWithDigest() digest = %q, error = %v, want error containing %q", digested, err, test.want)
			}
			if digested != "" {
				t.Fatalf("applyCopyAddWithDigest() digest = %q after error, want empty", digested)
			}
		})
	}
}
