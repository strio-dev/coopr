package oci

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/content"
	orasoci "oras.land/oras-go/v2/content/oci"
)

func readLayerMetadata(ctx context.Context, source content.ReadOnlyStorage, desc v1.Descriptor) ([]byte, error) {
	if desc.Size < 0 || desc.Size > maxMetadataBytes {
		return nil, errors.New("OCI metadata exceeds size limit")
	}
	r, err := source.Fetch(ctx, desc)
	if err != nil {
		return nil, err
	}
	defer func() { _ = r.Close() }()
	return io.ReadAll(io.LimitReader(&contextReader{ctx: ctx, reader: r}, maxMetadataBytes+1))
}

func decodeLayerObject(data []byte) (map[string]json.RawMessage, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil || object == nil {
		return nil, fmt.Errorf("invalid OCI metadata object: %v", err)
	}
	return object, nil
}

func layoutRoot(path string) (map[string]json.RawMessage, v1.Descriptor, error) {
	data, err := os.ReadFile(filepath.Join(path, "index.json"))
	if err != nil {
		return nil, v1.Descriptor{}, err
	}
	if len(data) > maxMetadataBytes {
		return nil, v1.Descriptor{}, errors.New("OCI index exceeds size limit")
	}
	index, err := decodeLayerObject(data)
	if err != nil {
		return nil, v1.Descriptor{}, err
	}
	var roots []v1.Descriptor
	if err := json.Unmarshal(index["manifests"], &roots); err != nil || len(roots) != 1 {
		return nil, v1.Descriptor{}, errors.New("OCI index does not contain exactly one root descriptor")
	}
	return index, roots[0], nil
}

// SetLayoutCreatedAnnotation records SOURCE_DATE_EPOCH on the sole root
// descriptor without changing the manifest or its digest.
func SetLayoutCreatedAnnotation(ctx context.Context, layout string, epoch *int64) error {
	if epoch == nil {
		return nil
	}
	return ConfigureLayoutCreatedAnnotation(ctx, layout, epoch, nil)
}

// ConfigureLayoutCreatedAnnotation applies the explicit --created-annotation
// state to the layout descriptor.  A nil state preserves Buildah's default;
// false removes inherited/generated values, including descriptors later copied
// into a multi-platform index; true derives the value from the image config
// when no timestamp override was supplied.
func ConfigureLayoutCreatedAnnotation(ctx context.Context, layout string, epoch *int64, enabled *bool) error {
	if enabled == nil && epoch == nil {
		return nil
	}
	index, _, err := layoutRoot(layout)
	if err != nil {
		return err
	}
	var roots []map[string]json.RawMessage
	if err := json.Unmarshal(index["manifests"], &roots); err != nil || len(roots) != 1 {
		return errors.New("OCI index does not contain exactly one root descriptor")
	}
	annotations := map[string]string{}
	if raw, present := roots[0]["annotations"]; present {
		if err := json.Unmarshal(raw, &annotations); err != nil {
			return fmt.Errorf("invalid root descriptor annotations: %w", err)
		}
	}
	if enabled != nil && !*enabled {
		delete(annotations, v1.AnnotationCreated)
	} else {
		created := ""
		if epoch != nil {
			created = time.Unix(*epoch, 0).UTC().Format(time.RFC3339)
		} else {
			raw, readErr := ReadImageConfigLayout(ctx, layout)
			if readErr != nil {
				return readErr
			}
			var config struct {
				Created *time.Time `json:"created"`
			}
			if err := json.Unmarshal(raw, &config); err != nil {
				return fmt.Errorf("decode image creation time: %w", err)
			}
			if config.Created != nil {
				created = config.Created.UTC().Format(time.RFC3339)
			}
		}
		if created != "" {
			annotations[v1.AnnotationCreated] = created
		}
	}
	if len(annotations) == 0 {
		delete(roots[0], "annotations")
	} else {
		roots[0]["annotations"], err = json.Marshal(annotations)
		if err != nil {
			return err
		}
	}
	index["manifests"], err = json.Marshal(roots)
	if err != nil {
		return err
	}
	data, err := json.Marshal(index)
	if err != nil {
		return err
	}
	temp, err := os.CreateTemp(layout, ".coopr-index-*.json")
	if err != nil {
		return err
	}
	name := temp.Name()
	defer func() { _ = os.Remove(name) }()
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return os.Rename(name, filepath.Join(layout, "index.json"))
}

// ReplaceImageAnnotationsLayout updates annotations on the OCI image manifest
// selected by the layout's sole root descriptor. Docker schema-2 manifests do
// not support annotations and are left unchanged.
func ReplaceImageAnnotationsLayout(ctx context.Context, layout string, inherit bool, set, unset []string) (v1.Descriptor, error) {
	index, root, err := layoutRoot(layout)
	if err != nil {
		return v1.Descriptor{}, err
	}
	if root.MediaType != v1.MediaTypeImageManifest {
		return root, nil
	}
	store, err := orasoci.New(layout)
	if err != nil {
		return v1.Descriptor{}, err
	}
	manifestBytes, err := readLayerMetadata(ctx, store, root)
	if err != nil {
		return v1.Descriptor{}, err
	}
	manifest, err := decodeLayerObject(manifestBytes)
	if err != nil {
		return v1.Descriptor{}, err
	}
	annotations := make(map[string]string)
	if inherit {
		if raw, present := manifest["annotations"]; present {
			if err := json.Unmarshal(raw, &annotations); err != nil {
				return v1.Descriptor{}, fmt.Errorf("invalid image manifest annotations: %w", err)
			}
		}
	}
	for _, item := range set {
		key, value, found := strings.Cut(item, "=")
		if !found || key == "" {
			return v1.Descriptor{}, fmt.Errorf("invalid image annotation %q", item)
		}
		annotations[key] = value
	}
	for _, key := range unset {
		delete(annotations, key)
	}
	if len(annotations) == 0 {
		delete(manifest, "annotations")
	} else {
		manifest["annotations"], err = json.Marshal(annotations)
		if err != nil {
			return v1.Descriptor{}, err
		}
	}
	updatedManifest, err := json.Marshal(manifest)
	if err != nil {
		return v1.Descriptor{}, err
	}
	if bytes.Equal(updatedManifest, manifestBytes) {
		return root, nil
	}
	var config v1.Descriptor
	if err := json.Unmarshal(manifest["config"], &config); err != nil {
		return v1.Descriptor{}, fmt.Errorf("invalid image config descriptor: %w", err)
	}
	configBytes, err := readLayerMetadata(ctx, store, config)
	if err != nil {
		return v1.Descriptor{}, err
	}
	updatedRoot := Descriptor(root.MediaType, updatedManifest)
	if err := writeNormalizedLayout(ctx, store, layout, index, updatedRoot, config, configBytes, updatedManifest); err != nil {
		return v1.Descriptor{}, err
	}
	return updatedRoot, nil
}

func writeNormalizedLayout(ctx context.Context, store *orasoci.Store, layout string, index map[string]json.RawMessage, root, config v1.Descriptor, configBytes, manifestBytes []byte) error {
	store.AutoSaveIndex = false
	for _, blob := range []struct {
		desc v1.Descriptor
		data []byte
	}{{config, configBytes}, {root, manifestBytes}} {
		exists, err := store.Exists(ctx, blob.desc)
		if err != nil {
			return err
		}
		if !exists {
			if err := store.Push(ctx, blob.desc, bytes.NewReader(blob.data)); err != nil {
				return err
			}
		}
	}
	var roots []map[string]json.RawMessage
	if err := json.Unmarshal(index["manifests"], &roots); err != nil || len(roots) != 1 {
		return errors.New("OCI index does not contain exactly one root descriptor")
	}
	var err error
	if roots[0]["digest"], err = json.Marshal(root.Digest); err != nil {
		return err
	}
	if roots[0]["size"], err = json.Marshal(root.Size); err != nil {
		return err
	}
	if roots[0]["mediaType"], err = json.Marshal(root.MediaType); err != nil {
		return err
	}
	delete(roots[0], "data")
	delete(roots[0], "urls")
	if rawAnnotations, present := roots[0]["annotations"]; present {
		var annotations map[string]string
		if err := json.Unmarshal(rawAnnotations, &annotations); err != nil {
			return fmt.Errorf("invalid root descriptor annotations: %w", err)
		}
		if _, present := annotations["config.digest"]; present {
			annotations["config.digest"] = config.Digest.String()
			if roots[0]["annotations"], err = json.Marshal(annotations); err != nil {
				return err
			}
		}
	}
	if index["manifests"], err = json.Marshal(roots); err != nil {
		return err
	}
	data, err := json.Marshal(index)
	if err != nil {
		return err
	}
	temp, err := os.CreateTemp(layout, ".coopr-index-*.json")
	if err != nil {
		return err
	}
	name := temp.Name()
	defer func() { _ = os.Remove(name) }()
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return os.Rename(name, filepath.Join(layout, "index.json"))
}
