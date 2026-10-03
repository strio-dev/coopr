package buildah

import (
	"context"
	"errors"

	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

// ImportLayoutSupervised retains a verified layout image in the same storage
// namespace used for builds and inputs. It returns the committed config ID.
func ImportLayoutSupervised(ctx context.Context, store StoreOptions, layout string, manifest v1.Descriptor) (string, error) {
	if ctx == nil {
		return "", errors.New("import context is nil")
	}
	if manifest.Digest == "" || manifest.Digest.Validate() != nil || manifest.Size < 0 {
		return "", errors.New("import requires a valid manifest descriptor")
	}
	response, err := runPlanSupervised(ctx, nil, SupervisedPlanOptions{
		Store: store, Output: Output{Path: layout}, ManifestDescriptor: manifest,
	}, "import")
	if err != nil {
		return "", err
	}
	return response.Result.ImageID, nil
}
