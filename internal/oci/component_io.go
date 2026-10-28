package oci

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"coopr/internal/componentstore"
	"coopr/internal/storeactivity"

	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/content"
	orasoci "oras.land/oras-go/v2/content/oci"
)

// PullComponent stores the entire component graph, including every indexed
// platform. The registry transport and local graph commit are shared with builds.
func (r *Resolver) PullComponent(ctx context.Context, reference, tag string) (root v1.Descriptor, retErr error) {
	if err := componentstore.ValidateTag(tag); err != nil {
		return root, err
	}
	ref, err := ParseReference(reference)
	if err != nil {
		return root, err
	}
	source, err := r.repository(ref)
	if err != nil {
		return root, err
	}
	root, err = source.Resolve(ctx, ref.Reference)
	if err != nil {
		return root, err
	}
	binding, err := r.componentBinding(ctx, source, root, ref.Registry+"/"+ref.Repository)
	if err != nil {
		return root, err
	}
	return r.storeComponentWithBinding(ctx, source, root, tag, binding)
}

// LoadComponent reads a standard OCI archive through ORAS without extracting
// arbitrary archive paths or interpreting package layers as runnable images.
func (r *Resolver) LoadComponent(ctx context.Context, path, tag string) (v1.Descriptor, error) {
	if err := componentstore.ValidateTag(tag); err != nil {
		return v1.Descriptor{}, err
	}
	roots, err := componentstore.ArchiveRoots(path)
	if err != nil {
		return v1.Descriptor{}, err
	}
	source, err := orasoci.NewFromTar(ctx, path)
	if err != nil {
		return v1.Descriptor{}, err
	}
	root, _, err := componentstore.ArchiveRoot(ctx, source, roots)
	if err != nil {
		return root, err
	}
	if tag == "" {
		tag = root.Annotations[v1.AnnotationRefName]
		if err := componentstore.ValidateTag(tag); err != nil {
			return root, err
		}
	}
	binding, err := r.componentBindingFromArchive(ctx, source, root, roots)
	if err != nil {
		return root, err
	}
	return r.storeComponentWithBinding(ctx, source, root, tag, binding)
}

func (r *Resolver) storeComponent(ctx context.Context, source content.ReadOnlyStorage, root v1.Descriptor, tag string) (_ v1.Descriptor, retErr error) {
	return r.storeComponentWithBinding(ctx, source, root, tag, nil)
}

func (r *Resolver) storeComponentWithBinding(ctx context.Context, source content.ReadOnlyStorage, root v1.Descriptor, tag string, binding *componentstore.Binding) (_ v1.Descriptor, retErr error) {
	activity, err := storeactivity.AcquireShared(ctx, r.componentStoreDir)
	if err != nil {
		return root, err
	}
	defer func() { retErr = errors.Join(retErr, activity.Close()) }()
	if err := r.validateComponentGraph(ctx, source, root); err != nil {
		return root, err
	}
	if err := componentstore.PutWithBinding(ctx, r.componentStoreDir, source, root, tag, binding); err != nil {
		return root, err
	}
	return root, nil
}

func (r *Resolver) validateComponentGraph(ctx context.Context, source content.ReadOnlyStorage, root v1.Descriptor) error {
	if err := checkDescriptor(root); err != nil {
		return err
	}
	if root.MediaType == v1.MediaTypeImageIndex {
		data, err := fetchMetadata(ctx, source, root)
		if err != nil {
			return err
		}
		var index v1.Index
		if err := json.Unmarshal(data, &index); err != nil {
			return err
		}
		if index.SchemaVersion != 2 || len(index.Manifests) == 0 || index.MediaType != "" && index.MediaType != root.MediaType {
			return fmt.Errorf("invalid component index")
		}
		for _, descriptor := range index.Manifests {
			if descriptor.Platform == nil {
				return fmt.Errorf("component index member %s has no platform", descriptor.Digest)
			}
			if _, err := r.resolveRoot(ctx, source, "", "", root, *descriptor.Platform, Component); err != nil {
				return err
			}
		}
	} else {
		data, err := fetchMetadata(ctx, source, root)
		if err != nil {
			return err
		}
		var manifest v1.Manifest
		if err := json.Unmarshal(data, &manifest); err != nil {
			return err
		}
		config, err := fetchMetadata(ctx, source, manifest.Config)
		if err != nil {
			return err
		}
		var metadata ComponentMetadata
		if err := json.Unmarshal(config, &metadata); err != nil {
			return err
		}
		if _, err := r.resolveRoot(ctx, source, "", "", root, metadata.Platform, Component); err != nil {
			return err
		}
	}
	return verifyArchiveGraph(ctx, source, root)
}
