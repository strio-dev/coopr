package oci

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"

	"github.com/strio-dev/coopr/internal/componentstore"

	digest "github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale"
	"oras.land/oras-go/v2/content"
	orasoci "oras.land/oras-go/v2/content/oci"
	"oras.land/oras-go/v2/registry"
)

// addAuthoredComponent retains the existing native dependency discovery. It
// registers only an authored root/index members and the declaration Bale prepares;
// ordinary payload discovery and copying remain with Bale.
func (s *componentBundleSources) addAuthoredComponent(ctx context.Context, name, selector string, store *orasoci.ReadOnlyStore, root v1.Descriptor, policies map[string]bale.DependencyPolicy) error {
	root.Annotations = maps.Clone(root.Annotations)
	delete(root.Annotations, v1.AnnotationRefName)
	repo := s.open(name)
	if err := repo.addSelector(selector, root); err != nil {
		return err
	}
	repo.addStore(store)
	queue := []v1.Descriptor{root}
	for len(queue) > 0 {
		next := queue[0]
		queue = queue[1:]
		id := bale.ArtifactID{Repository: name, Digest: next.Digest}
		if s.authored[id] {
			continue
		}
		if selected := repo.aliasSubjects[next.Digest]; selected != nil {
			return fmt.Errorf("component %s@%s has both authored and delivered selections", name, next.Digest)
		}
		s.authored[id] = true
		repo.aliasSubjects[next.Digest] = nil
		if err := repo.addSelector(next.Digest.String(), next); err != nil {
			return err
		}
		if next.MediaType == v1.MediaTypeImageIndex {
			raw, err := fetchMetadata(ctx, store, next)
			if err != nil {
				return err
			}
			var index v1.Index
			if err := json.Unmarshal(raw, &index); err != nil {
				return err
			}
			queue = append(queue, index.Manifests...)
		}
	}
	declaration, incomplete, err := prepareComponentDependencies(ctx, store, root, name, policies)
	if err != nil {
		return err
	}
	if incomplete {
		return errors.New("component dependency discovery is incomplete; a complete bundle requires registry-qualified static dependencies")
	}
	if declaration == nil {
		return nil
	}
	repo.stores = append(repo.stores, declaration.Store)
	raw, err := fetchMetadata(ctx, declaration.Store, declaration.Descriptor)
	if err != nil {
		return err
	}
	var manifest v1.Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return err
	}
	for _, descriptor := range append([]v1.Descriptor{declaration.Descriptor, manifest.Config}, manifest.Layers...) {
		if err := repo.addSelector(descriptor.Digest.String(), descriptor); err != nil {
			return err
		}
	}
	repo.addReferrer(root, declaration.Descriptor)
	return nil
}

func (r *Resolver) addBundleComponents(ctx context.Context, client *bale.Client, sources *componentBundleSources, components map[string]string, recipe *bale.Recipe) (map[string]bale.DependencyPolicy, error) {
	type authoredComponent struct {
		ref   registry.Reference
		root  v1.Descriptor
		store *orasoci.ReadOnlyStore
	}
	var authored []authoredComponent
	policies := make(map[string]bale.DependencyPolicy, len(components))
	for _, reference := range slices.Sorted(maps.Keys(components)) {
		canonical, ok := registryInput("component", reference)
		if !ok {
			return nil, fmt.Errorf("selected child %q requires a qualified immutable coordinate", reference)
		}
		ref, err := registry.ParseReference(canonical)
		if err != nil || digest.Digest(ref.Reference).Validate() != nil {
			return nil, fmt.Errorf("selected child %q requires a qualified immutable coordinate", reference)
		}
		if _, exists := policies[canonical]; exists {
			return nil, fmt.Errorf("selected child %q has multiple local selections", canonical)
		}
		store, root, binding, err := componentstore.Resolve(ctx, r.componentStoreDir, components[reference])
		if err != nil {
			return nil, fmt.Errorf("selected child %s: %w", reference, err)
		}
		if root.Digest.String() != ref.Reference {
			return nil, fmt.Errorf("selected child %s differs from local root %s", reference, root.Digest)
		}
		if err := r.validateComponentGraph(ctx, store, root); err != nil {
			return nil, err
		}
		if binding == nil {
			policies[canonical] = bale.DependencyPolicy{Follow: "auto"}
			authored = append(authored, authoredComponent{ref: ref, root: root, store: store})
			continue
		}
		records, err := inspectRetainedDestinations(ctx, store, root, binding)
		if err != nil {
			return nil, err
		}
		delivery, err := selectComponentDelivery(records, binding.Repository, root, nil)
		if err != nil {
			return nil, err
		}
		if delivery == nil {
			return nil, fmt.Errorf("selected child %s has no original delivery selection", reference)
		}
		var record bale.InspectedDestination
		for _, candidate := range records {
			if content.Equal(candidate.Descriptor, delivery.Attachment) {
				record = candidate
				break
			}
		}
		if err := sources.matchDeliveryCapture(ctx, client, record, delivery); err != nil {
			return nil, fmt.Errorf("selected child %s: %w", reference, err)
		}
		source, err := registry.ParseReference(record.Record.Source)
		if err != nil {
			return nil, err
		}
		original := sources.repositories[source.Registry+"/"+source.Repository]
		if original == nil {
			return nil, errors.New("matched capture has no original component repository")
		}
		aliasName := ref.Registry + "/" + ref.Repository
		alias := sources.open(aliasName)
		if alias != original {
			if selected, exists := alias.aliasSubjects[root.Digest]; exists && selected != original {
				return nil, fmt.Errorf("component %s@%s has conflicting local selections", aliasName, root.Digest)
			}
			alias.aliasSubjects[root.Digest] = original
			alias.aliasName = aliasName
			if !slices.Contains(alias.aliasSources, original) {
				alias.aliasSources = append(alias.aliasSources, original)
				alias.stores = append(alias.stores, original.stores...)
				alias.resolvers = append(alias.resolvers, original.resolvers...)
			}
		}
		if err := alias.addSelector(ref.Reference, original.selectors[root.Digest.String()]); err != nil {
			return nil, err
		}
		policies[canonical] = delivery.Scope.Policy
		// Keep the exact original attachment as provenance. Its pinned Bundle is
		// metadata, not an OCI graph edge: making that whole capture a root would
		// copy unrelated inputs. Bale follows only the selected new dependency graph.
		attachmentRef := binding.Repository + "@" + delivery.Attachment.Digest.String()
		provenance := sources.open(binding.Repository)
		provenance.addStore(store)
		provenance.aliasSubjects[delivery.Attachment.Digest] = nil
		if err := provenance.addSelector(delivery.Attachment.Digest.String(), delivery.Attachment); err != nil {
			return nil, err
		}
		if !slices.ContainsFunc(recipe.Include, func(selection bale.Selection) bool { return selection.Ref == attachmentRef }) {
			recipe.Include = append(recipe.Include, bale.Selection{Ref: attachmentRef, Referrers: bale.ReferrerPolicy{Include: "none"}, Dependencies: bale.DependencyPolicy{Follow: "none"}})
		}
		if err := provenance.addObservation(bale.DependencyResolution{Repository: binding.Repository, Subject: delivery.Attachment, Policy: bale.DependencyPolicy{Follow: "none"}, Outcome: "disabled", Dependencies: []bale.ResolvedDependency{}}); err != nil {
			return nil, err
		}
	}
	for _, input := range authored {
		if err := sources.addAuthoredComponent(ctx, input.ref.Registry+"/"+input.ref.Repository, input.ref.Reference, input.store, input.root, policies); err != nil {
			return nil, err
		}
	}
	return policies, nil
}

func (s *componentBundleSources) matchDeliveryCapture(ctx context.Context, client *bale.Client, record bale.InspectedDestination, delivery *ComponentDelivery) error {
	bundle, err := registry.ParseReference(delivery.Scope.Bundle)
	if err != nil {
		return err
	}
	for i := range s.captures {
		capture := &s.captures[i]
		if capture.root.Digest.String() != bundle.Reference {
			continue
		}
		if capture.plan == nil {
			plan, err := client.PlanPush(ctx, capture.path, "coopr.invalid/preflight:bundle", bale.PushOptions{})
			if err != nil {
				return err
			}
			if plan.MetadataOnly {
				return errors.New("selected delivery requires a complete materialized Bale capture")
			}
			capture.plan = &plan
		}
		for _, planned := range capture.plan.Destinations {
			if planned.Record.Source != record.Record.Source || !content.Equal(planned.Subject, delivery.Subject) {
				continue
			}
			for _, resolution := range planned.Record.Resolutions {
				if equivalentCapturedResolution(delivery.Resolution, resolution) {
					return nil
				}
			}
		}
	}
	return errors.New("no complete input bundle matches the selected original source, capture, policy and dependencies")
}

func equivalentCapturedResolution(a, b bale.DestinationResolution) bool {
	clearPlacement := func(value bale.DestinationResolution) bale.DestinationResolution {
		value = cloneDeliveryValue(value)
		for i := range value.Dependencies {
			value.Dependencies[i].Destination = ""
		}
		slices.SortFunc(value.Dependencies, func(a, b bale.DestinationDependency) int {
			left, _ := json.Marshal(a)
			right, _ := json.Marshal(b)
			return strings.Compare(string(left), string(right))
		})
		return value
	}
	return reflect.DeepEqual(clearPlacement(a), clearPlacement(b))
}
