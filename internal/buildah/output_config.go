package buildah

import (
	"context"
	"fmt"

	"coopr/internal/imageconfig"
	"coopr/internal/oci"
	"go.podman.io/image/v5/types"
)

// reconcileOutputConfig preserves the full logical config while adopting the
// layer and history provenance emitted by Buildah. Both normal and finalized
// images use the same import path into the canonical storage graph.
func (executor *graphExecutor) reconcileOutputConfig(ctx context.Context, committed Result, system *types.SystemContext, logical *imageconfig.Config, historyOnlyTail bool, executorProvenance ...[]byte) (Result, error) {
	var emitted []byte
	var err error
	if len(executorProvenance) != 0 && len(executorProvenance[0]) != 0 {
		emitted = executorProvenance[0]
	} else {
		emitted, err = oci.ReadImageConfigLayout(ctx, committed.Layout)
		if err != nil {
			return Result{}, fmt.Errorf("read committed image config: %w", err)
		}
	}
	zeroLayer, inspectErr := layoutImageHasNoLayers(committed.Layout)
	if inspectErr != nil {
		return Result{}, fmt.Errorf("inspect committed image layers: %w", inspectErr)
	}
	emitted, err = normalizeZeroLayerExecutorConfig(emitted, zeroLayer)
	if err != nil {
		return Result{}, fmt.Errorf("normalize committed image config: %w", err)
	}
	if historyOnlyTail {
		emitted, err = preserveLogicalCreated(emitted, logical)
		if err != nil {
			return Result{}, fmt.Errorf("preserve metadata-only image creation time: %w", err)
		}
	}
	if err := logical.AdoptExecutorProvenance(emitted); err != nil {
		return Result{}, fmt.Errorf("reconcile committed image config: %w", err)
	}
	controls := effectiveOutputControls(executor.options.ImageControls, timestampPolicyFromOptions(executor.options))
	if err := logical.ApplyOutput(controls); err != nil {
		return Result{}, fmt.Errorf("apply output image controls: %w", err)
	}
	raw, err := logical.MarshalJSON()
	if err != nil {
		return Result{}, fmt.Errorf("marshal committed image config: %w", err)
	}
	configDigest, _, err := oci.ReplaceImageConfigLayout(ctx, committed.Layout, raw)
	if err != nil {
		return Result{}, fmt.Errorf("replace committed image config: %w", err)
	}
	if err := oci.ConfigureLayoutCreatedAnnotation(ctx, committed.Layout, timestampPolicyFromOptions(executor.options).createdEpoch(), controls.CreatedAnnotation); err != nil {
		return Result{}, fmt.Errorf("set image created annotation: %w", err)
	}
	controls = executor.options.ImageControls
	if controls.DropInheritedAnnotations || len(controls.Annotations) != 0 || len(controls.UnsetAnnotations) != 0 {
		_, err = oci.ReplaceImageAnnotationsLayout(ctx, committed.Layout, !controls.DropInheritedAnnotations, controls.Annotations, controls.UnsetAnnotations)
		if err != nil {
			return Result{}, fmt.Errorf("apply output image annotations: %w", err)
		}
	}
	selected, err := oci.LayoutRoot(committed.Layout)
	if err != nil {
		return Result{}, fmt.Errorf("read final image manifest: %w", err)
	}
	committed.ImageID, err = ImportSelectedImage(ctx, executor.store, system, committed.Layout, selected)
	if err != nil {
		return Result{}, fmt.Errorf("import final image into image graph: %w", err)
	}
	if committed.ImageID != configDigest.Encoded() {
		return Result{}, fmt.Errorf("imported final image ID %s differs from config %s", committed.ImageID, configDigest)
	}
	committed.ManifestDigest = selected.Digest.String()
	return committed, nil
}
