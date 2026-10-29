package componentstore

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"

	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/coopr/internal/localstore"
	"oras.land/oras-go/v2/content"
)

const maxArchiveMetadata = 8 << 20

func archiveDescriptors(root v1.Descriptor) (v1.Descriptor, []v1.Descriptor, error) {
	binding, err := BindingFromDescriptor(root)
	if err != nil {
		return root, nil, err
	}
	root.Annotations = maps.Clone(root.Annotations)
	delete(root.Annotations, bindingAnnotation)
	delete(root.Annotations, bindingHistoryAnnotation)
	delete(root.Annotations, localstore.AuxiliaryRootOwner)
	if binding == nil {
		return root, nil, nil
	}
	if root.Annotations == nil {
		root.Annotations = map[string]string{}
	}
	root.Annotations[archiveRepositoryAnnotation] = binding.Repository
	auxiliary := make([]v1.Descriptor, len(binding.Attachments))
	for i, descriptor := range binding.Attachments {
		descriptor.Annotations = maps.Clone(descriptor.Annotations)
		delete(descriptor.Annotations, localstore.AuxiliaryRootOwner)
		delete(descriptor.Annotations, v1.AnnotationRefName)
		auxiliary[i] = descriptor
	}
	return root, auxiliary, nil
}

// WriteArchiveTo preserves the unchanged component plus its original delivery
// attachment graphs. It never exports private local binding history.
func WriteArchiveTo(ctx context.Context, source content.ReadOnlyStorage, root v1.Descriptor, writer io.Writer) error {
	root, auxiliary, err := archiveDescriptors(root)
	if err != nil {
		return err
	}
	return localstore.WriteArchiveTo(ctx, source, root, writer, auxiliary...)
}

// ArchiveRoots reads only the OCI index, without extracting archive paths.
func ArchiveRoots(path string) ([]v1.Descriptor, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	reader := tar.NewReader(file)
	for {
		header, err := reader.Next()
		if err != nil {
			return nil, fmt.Errorf("OCI archive has no index.json: %w", err)
		}
		if header.Name != "index.json" && header.Name != "./index.json" {
			continue
		}
		if header.Size < 0 || header.Size > maxArchiveMetadata {
			return nil, errors.New("OCI index is too large")
		}
		var index v1.Index
		if err := json.NewDecoder(io.LimitReader(reader, header.Size)).Decode(&index); err != nil {
			return nil, fmt.Errorf("decode OCI index: %w", err)
		}
		if index.SchemaVersion != 2 || len(index.Manifests) == 0 || index.MediaType != "" && index.MediaType != v1.MediaTypeImageIndex {
			return nil, errors.New("invalid OCI archive index")
		}
		return index.Manifests, nil
	}
}

// ArchiveRoot selects one subjectless component graph; its auxiliaries must be
// original attachment manifests for that exact root. The caller validates the
// component and Bale records before exposing a local name.
func ArchiveRoot(ctx context.Context, source content.ReadOnlyStorage, roots []v1.Descriptor) (v1.Descriptor, *Binding, error) {
	var root v1.Descriptor
	var attachments []v1.Descriptor
	var subjects []v1.Descriptor
	seen := map[string]bool{}
	for _, descriptor := range roots {
		if descriptor.Digest.Validate() != nil || descriptor.Size < 0 || descriptor.Size > maxArchiveMetadata {
			return root, nil, fmt.Errorf("invalid OCI archive descriptor %s", descriptor.Digest)
		}
		if seen[descriptor.Digest.String()] {
			return root, nil, fmt.Errorf("duplicate OCI archive root %s", descriptor.Digest)
		}
		seen[descriptor.Digest.String()] = true
		if descriptor.MediaType == v1.MediaTypeImageIndex {
			if root.Digest != "" {
				return root, nil, errors.New("component archive must contain exactly one component root")
			}
			root = descriptor
			continue
		}
		if descriptor.MediaType != v1.MediaTypeImageManifest {
			return root, nil, fmt.Errorf("unsupported component archive root media type %q", descriptor.MediaType)
		}
		data, err := content.FetchAll(ctx, source, descriptor)
		if err != nil {
			return root, nil, err
		}
		var manifest v1.Manifest
		if err := json.Unmarshal(data, &manifest); err != nil || manifest.SchemaVersion != 2 {
			return root, nil, errors.New("invalid component archive manifest")
		}
		if manifest.Subject == nil {
			if root.Digest != "" {
				return root, nil, errors.New("component archive must contain exactly one component root")
			}
			root = descriptor
		} else {
			attachments = append(attachments, descriptor)
			subjects = append(subjects, *manifest.Subject)
		}
	}
	if root.Digest == "" {
		return root, nil, errors.New("component archive must contain exactly one component root")
	}
	for _, subject := range subjects {
		if !content.Equal(subject, root) {
			return root, nil, errors.New("component archive attachment names a different subject")
		}
	}
	repository := root.Annotations[archiveRepositoryAnnotation]
	if repository == "" {
		if len(attachments) > 0 {
			return root, nil, errors.New("component archive attachments have no repository context")
		}
		root, err := withBinding(root, nil)
		return root, nil, err
	}
	binding, err := normalizeBinding(&Binding{Repository: repository, Attachments: attachments})
	if err != nil {
		return root, nil, err
	}
	root, err = withBinding(root, binding)
	if root.Annotations != nil {
		delete(root.Annotations, archiveRepositoryAnnotation)
	}
	return root, binding, err
}
