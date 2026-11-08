package buildah

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	digest "github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	upstream "go.podman.io/buildah"
	"go.podman.io/image/v5/types"
	"go.podman.io/storage"
)

func instructionGraphBoundary(builder *upstream.Builder, root *PackageRootMetadata, platform v1.Platform) (digest.Digest, error) {
	sourceRootFS := builder.OCIv1.RootFS
	if builder.FromImageID == "" && sourceRootFS.Type == "" && len(sourceRootFS.DiffIDs) == 0 {
		sourceRootFS.Type = "layers"
	}
	identity, err := rootFSIdentity(sourceRootFS)
	if err != nil {
		return "", err
	}
	// A new scratch directory's wall-clock timestamp is not authored input.
	if builder.FromImageID == "" {
		root = nil
	}
	if root != nil {
		var ok bool
		root, ok = cacheablePackageRootMetadata(root)
		if !ok {
			return "", errors.New("graph source root metadata is not cacheable")
		}
	}
	raw, err := json.Marshal(struct {
		Version, RootFS string
		Root            *PackageRootMetadata
		Platform        v1.Platform
	}{instructionSolverVersion, identity, root, platform})
	if err != nil {
		return "", err
	}
	return digest.FromBytes(raw), nil
}

// A relayed completed stage is a ready immutable image, not an operation that
// this worker can execute again. Pin its actual state before the next solve;
// otherwise the solver may select a newer alternate producer result that does
// not match the builder already prepared by the stage scheduler.
func (s *instructionSolver) pinCompletedGraphParent(builder *upstream.Builder, root *PackageRootMetadata, platform v1.Platform, parent *instructionVertex, force bool) (*instructionVertex, error) {
	if parent == nil || parent.source == nil || parent.parent == nil {
		return parent, nil
	}
	boundary, err := instructionGraphBoundary(builder, root, platform)
	if err != nil {
		return nil, err
	}
	for ancestor := parent; ancestor != nil; ancestor = ancestor.parent {
		force = force || ancestor.ignore
	}
	return s.source(boundary, *parent.source, force), nil
}

func verifyInstructionParent(ctx context.Context, store storage.Store, system *types.SystemContext, builder *upstream.Builder, parent *instructionSolverResult) error {
	if parent == nil {
		return errors.New("missing selected parent")
	}
	if parent.Entry.ImageID == "" {
		if len(builder.OCIv1.RootFS.DiffIDs) != 0 {
			return errors.New("scratch graph parent has nonempty rootfs")
		}
		return nil
	}
	raw, err := packageImageConfigSelected(ctx, store, parent.Entry.ImageID, system, parent.Entry.ManifestDigest)
	if err != nil {
		return err
	}
	var image v1.Image
	if err = json.Unmarshal(raw, &image); err != nil {
		return err
	}
	if !slices.Equal(builder.OCIv1.RootFS.DiffIDs, image.RootFS.DiffIDs) {
		return fmt.Errorf("mounted parent differs from solver-selected complete image: mounted %v, selected %v (image %s, manifest %s)", builder.OCIv1.RootFS.DiffIDs, image.RootFS.DiffIDs, parent.Entry.ImageID, parent.Entry.ManifestDigest)
	}
	return nil
}

// replaceGraphCachedMetadata keeps the selected image's complete physical
// ancestry and reapplies current authored empty history. Only history entries
// describing the selected physical layers come from the cached result.
func replaceGraphCachedMetadata(builder *upstream.Builder, replacement cachedMetadataReplacement, createdBy string, parentLayers int) (bool, error) {
	if parentLayers < 0 || parentLayers > len(builder.OCIv1.RootFS.DiffIDs) {
		return false, errors.New("invalid selected parent layer count")
	}
	if parentLayers != len(replacement.ParentDiffIDs) {
		return false, errors.New("selected parent and authored history have different layer shapes")
	}
	if slices.Equal(replacement.ParentDiffIDs, builder.OCIv1.RootFS.DiffIDs[:parentLayers]) {
		return replaceCachedMetadata(builder, replacement, createdBy)
	}
	selectedOCI := make([]v1.History, 0, parentLayers)
	selectedDocker := make([]int, 0, parentLayers)
	for i, h := range builder.OCIv1.History {
		if !h.EmptyLayer && len(selectedOCI) < parentLayers {
			selectedOCI = append(selectedOCI, h)
			selectedDocker = append(selectedDocker, i)
		}
	}
	if len(selectedOCI) != parentLayers {
		return false, errors.New("selected image history is missing parent filesystem layers")
	}
	if len(builder.Docker.History) != len(builder.OCIv1.History) {
		return false, errors.New("selected OCI and Docker history lengths differ")
	}
	replacement.OCIBase = slices.Clone(replacement.OCIBase)
	replacement.DockerBase = slices.Clone(replacement.DockerBase)
	next := 0
	for i, h := range replacement.OCIBase {
		if !h.EmptyLayer {
			if next >= parentLayers {
				return false, errors.New("authored OCI history exceeds selected parent")
			}
			if replacement.ParentDiffIDs[next] != builder.OCIv1.RootFS.DiffIDs[next] {
				replacement.OCIBase[i] = selectedOCI[next]
			}
			next++
		}
	}
	if next != parentLayers {
		return false, errors.New("authored OCI history omits parent filesystem layers")
	}
	next = 0
	for i, h := range replacement.DockerBase {
		if !h.EmptyLayer {
			if next >= parentLayers {
				return false, errors.New("authored Docker history exceeds selected parent")
			}
			if replacement.ParentDiffIDs[next] != builder.OCIv1.RootFS.DiffIDs[next] {
				replacement.DockerBase[i] = builder.Docker.History[selectedDocker[next]]
			}
			next++
		}
	}
	if next != parentLayers {
		return false, errors.New("authored Docker history omits parent filesystem layers")
	}
	replacement.ParentDiffIDs = slices.Clone(builder.OCIv1.RootFS.DiffIDs[:parentLayers])
	changed, err := replaceCachedMetadata(builder, replacement, createdBy)
	if err != nil {
		return false, err
	}
	nonempty := 0
	for _, h := range builder.OCIv1.History {
		if !h.EmptyLayer {
			nonempty++
		}
	}
	if nonempty != len(builder.OCIv1.RootFS.DiffIDs) {
		return false, fmt.Errorf("selected image has %d filesystem history entries for %d layers", nonempty, len(builder.OCIv1.RootFS.DiffIDs))
	}
	return changed, nil
}
