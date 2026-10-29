package oci

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/strio-dev/coopr/internal/imageconfig"
	"github.com/strio-dev/coopr/internal/planner"

	digest "github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale"
	"oras.land/oras-go/v2/content"
)

// ComponentPackageBinds reads the immutable package configuration needed by
// invocation planning without materializing package filesystems.
func ComponentPackageBinds(metadata ComponentMetadata) (map[string]planner.StageBind, error) {
	projection, err := planner.ProjectPublished(&metadata.Component)
	if err != nil {
		return nil, err
	}
	packages := make(map[string]Package, len(metadata.Packages))
	for _, pkg := range metadata.Packages {
		packages[strings.ToLower(pkg.Stage)] = pkg
	}
	binds := make(map[string]planner.StageBind)
	for _, stage := range projection.Stages {
		binding := projection.FromBindings[stage.ID]
		if stage.Kind != "from" || binding.Kind != "stage" {
			continue
		}
		index, err := strconv.Atoi(binding.Stage)
		if err != nil || index < 0 || index >= len(projection.Stages) {
			return nil, fmt.Errorf("invalid base stage %q", binding.Stage)
		}
		base := projection.Stages[index]
		if base.Kind != "package" {
			continue
		}
		pkg, ok := packages[strings.ToLower(base.Name)]
		if !ok {
			return nil, fmt.Errorf("no package for stage %q", base.Name)
		}
		config, err := imageconfig.Parse(pkg.Config)
		if err != nil {
			return nil, fmt.Errorf("package %q configuration: %w", base.Name, err)
		}
		bind, err := planner.StageBindFromImageConfig(config)
		if err != nil {
			return nil, fmt.Errorf("package %q inherited instructions: %w", base.Name, err)
		}
		binds[stage.ID] = bind
	}
	return binds, nil
}

// prepareComponentDependencies reads only component metadata. Dependency
// selectors remain mutable here; Bale resolves them when capturing a bundle.
// Image roots and embedded package producers do not acquire declarations.
func prepareComponentDependencies(ctx context.Context, source content.ReadOnlyStorage, root v1.Descriptor, repository string) (*bale.AttachRequest, bool, error) {
	var dependencies []bale.DependencyReference
	incomplete := false
	inspected := make(map[digest.Digest]bool)
	var inspect func(v1.Descriptor) (bool, error)
	inspect = func(descriptor v1.Descriptor) (component bool, retErr error) {
		if component, ok := inspected[descriptor.Digest]; ok {
			return component, nil
		}
		defer func() {
			if retErr == nil {
				inspected[descriptor.Digest] = component
			}
		}()
		data, err := fetchMetadata(ctx, source, descriptor)
		if err != nil {
			return false, err
		}
		if descriptor.MediaType == v1.MediaTypeImageIndex {
			var index v1.Index
			if err := json.Unmarshal(data, &index); err != nil {
				return false, err
			}
			if len(index.Manifests) == 0 {
				return false, nil
			}
			for _, child := range index.Manifests {
				component, err := inspect(child)
				if err != nil || !component {
					return false, err
				}
			}
			return true, nil
		}
		if descriptor.MediaType != v1.MediaTypeImageManifest {
			return false, nil
		}
		var manifest v1.Manifest
		if err := json.Unmarshal(data, &manifest); err != nil {
			return false, err
		}
		if manifest.ArtifactType != ComponentArtifactType || manifest.Config.MediaType != ComponentConfigType {
			return false, nil
		}
		config, err := fetchMetadata(ctx, source, manifest.Config)
		if err != nil {
			return false, err
		}
		var metadata ComponentMetadata
		if err := json.Unmarshal(config, &metadata); err != nil {
			return false, err
		}
		if err := validateComponent(metadata, manifest.Layers, metadata.Platform); err != nil {
			return false, err
		}
		binds, err := ComponentPackageBinds(metadata)
		if err != nil {
			return false, err
		}
		observed, err := planner.InspectPublishedDependencies(&metadata.Component, binds)
		if err != nil {
			return false, err
		}
		incomplete = incomplete || observed.Incomplete
		for _, input := range observed.Inputs {
			if input.Kind != "component" && input.Kind != "image" {
				continue
			}
			// Local digests and non-registry transports have no repository
			// coordinate for a generic dependency declaration.
			if digest.Digest(input.Reference).Validate() == nil {
				incomplete = true
				continue
			}
			selector, ok := registryInput(input.Kind, input.Reference)
			if !ok {
				incomplete = true
				continue
			}
			dependencies = append(dependencies, bale.DependencyReference{Ref: selector})
		}
		return true, nil
	}
	component, err := inspect(root)
	if err != nil {
		return nil, false, err
	}
	if !component {
		return nil, false, nil
	}
	if len(dependencies) == 0 {
		return nil, incomplete, nil
	}
	request, err := bale.PrepareDependencyDeclaration(ctx, bale.Root{Repository: repository, Descriptor: root}, bale.DependencyDeclaration{Dependencies: dependencies})
	if err != nil {
		return nil, incomplete, err
	}
	return &request, incomplete, nil
}

func (r *Resolver) publishComponentDependencies(ctx context.Context, request *bale.AttachRequest, incomplete bool) error {
	if incomplete && r.progressWriter != nil {
		_, _ = fmt.Fprintln(r.progressWriter, "WARNING: component dependency discovery covers static/default references; include runtime-selected dependencies explicitly when creating a Bale bundle")
	}
	if request == nil {
		return nil
	}
	client, err := r.baleClient(ctx, request.Subject)
	if err != nil {
		return err
	}
	result, err := client.Attach(ctx, *request)
	if err != nil {
		return fmt.Errorf("dependency declaration for %s (published=%t, discoverable=%t): %w", request.Subject, result.Published, result.Discoverable, err)
	}
	if !result.Published || !result.Discoverable {
		return fmt.Errorf("dependency declaration for %s was not confirmed published and discoverable", request.Subject)
	}
	return nil
}
