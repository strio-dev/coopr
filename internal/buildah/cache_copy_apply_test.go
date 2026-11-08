package buildah

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	upstream "go.podman.io/buildah"
)

type cancelingCopyBuilder struct {
	recordingBuilder
	cancel context.CancelFunc
}

func (builder *cancelingCopyBuilder) add(destination string, extract bool, options upstream.AddAndCopyOptions, sources ...string) error {
	err := builder.recordingBuilder.add(destination, extract, options, sources...)
	builder.cancel()
	return err
}

func TestApplyCopyAddDigestCancellation(t *testing.T) {
	for _, before := range []bool{true, false} {
		t.Run(fmt.Sprint("before=", before), func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			builder := &cancelingCopyBuilder{cancel: cancel}
			if before {
				cancel()
			}
			input, err := applyCopyAddWithDigestContext(ctx, builder, t.TempDir(), nil, Copy{Sources: []string{"selected"}, Destination: "/selected"})
			if !errors.Is(err, context.Canceled) || input != "" {
				t.Fatalf("cancelled COPY returned input %q, error %v", input, err)
			}
			want := 1
			if before {
				want = 0
			}
			if len(builder.addOptions) != want {
				t.Fatalf("native calls = %d, want %d", len(builder.addOptions), want)
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
			digested, err := applyCopyAddWithDigestContext(t.Context(), nativeBuilder{}, test.context, test.artifacts, test.operation)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("applyCopyAddWithDigest() digest = %q, error = %v, want error containing %q", digested, err, test.want)
			}
			if digested != "" {
				t.Fatalf("applyCopyAddWithDigest() digest = %q after error, want empty", digested)
			}
		})
	}
}
