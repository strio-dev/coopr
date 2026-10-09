package build

import (
	"context"

	"coopr/internal/testutil"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"testing"
)

func readExampleImage(t *testing.T, ctx context.Context, archive string) (v1.Manifest, v1.Image) {
	return testutil.ReadImage(t, ctx, archive)
}
