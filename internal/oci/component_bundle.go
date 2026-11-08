package oci

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"slices"

	"github.com/strio-dev/coopr/internal/componentstore"
	"github.com/strio-dev/coopr/internal/storeactivity"

	digest "github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale"
	"oras.land/oras-go/v2/content"
	orasoci "oras.land/oras-go/v2/content/oci"
	"oras.land/oras-go/v2/errdef"
	"oras.land/oras-go/v2/registry"
)

type ComponentBundleOptions struct {
	Reference string
	Origin    string
	Output    string
	Inputs    []string
	// Components maps qualified immutable input coordinates to explicit local
	// names. Names choose an archive's binding; they are not published aliases.
	Components map[string]string
}

// BundleComponent captures a newly produced local component and its declared
// dependencies without a staging registry. Input layouts must be complete Bale
// captures; their locked selectors, not live registries, supply dependencies.
// Origin is an explicit registry coordinate for later Bale relocation, not a
// publication target. Existing delivered components retain their original bundle.
func (r *Resolver) BundleComponent(ctx context.Context, options ComponentBundleOptions) (_ bale.CreateResult, retErr error) {
	activity, err := storeactivity.AcquireShared(ctx, r.componentStoreDir)
	if err != nil {
		return bale.CreateResult{}, err
	}
	defer func() { retErr = errors.Join(retErr, activity.Close()) }()
	store, root, binding, err := componentstore.Resolve(ctx, r.componentStoreDir, options.Reference)
	if err != nil {
		return bale.CreateResult{}, err
	}
	if binding != nil {
		return bale.CreateResult{}, errors.New("component already has a delivery binding; retain and deliver its original complete Bale bundle")
	}
	return r.bundleComponent(ctx, store, root, options)
}

// BundleComponentLayout consumes the exact unbound output of the current build,
// avoiding name races and unrelated historical deliveries in the shared store.
func (r *Resolver) BundleComponentLayout(ctx context.Context, layout string, root v1.Descriptor, options ComponentBundleOptions) (_ bale.CreateResult, retErr error) {
	if options.Reference != "" {
		return bale.CreateResult{}, errors.New("component layout source cannot also select a local reference")
	}
	activity, err := storeactivity.AcquireShared(ctx, r.componentStoreDir)
	if err != nil {
		return bale.CreateResult{}, err
	}
	defer func() { retErr = errors.Join(retErr, activity.Close()) }()
	store, err := orasoci.NewFromFS(ctx, os.DirFS(layout))
	if err != nil {
		return bale.CreateResult{}, err
	}
	return r.bundleComponent(ctx, store, root, options)
}

func (r *Resolver) bundleComponent(ctx context.Context, store *orasoci.ReadOnlyStore, root v1.Descriptor, options ComponentBundleOptions) (bale.CreateResult, error) {
	if err := r.validateComponentGraph(ctx, store, root); err != nil {
		return bale.CreateResult{}, err
	}
	if options.Origin == "" {
		options.Origin = "coopr.invalid/components/" + root.Digest.Encoded() + "@" + root.Digest.String()
	}
	recipe, err := bale.NormalizeRecipe(bale.Recipe{Include: []bale.Selection{{Ref: options.Origin}}})
	if err != nil {
		return bale.CreateResult{}, err
	}
	ref, err := registry.ParseReference(recipe.Include[0].Ref)
	if err != nil {
		return bale.CreateResult{}, err
	}
	if pinned := digest.Digest(ref.Reference); pinned.Validate() == nil && pinned != root.Digest {
		return bale.CreateResult{}, errors.New("bundle origin digest differs from the local component root")
	}
	sources := newComponentBundleSources()
	client, err := bale.NewClient(bale.ClientOptions{RepositoryFactory: sources.repository})
	if err != nil {
		return bale.CreateResult{}, err
	}
	for _, input := range options.Inputs {
		if err := sources.addLayout(ctx, client, input); err != nil {
			return bale.CreateResult{}, fmt.Errorf("dependency bundle %s: %w", input, err)
		}
	}
	policies, err := r.addBundleComponents(ctx, client, sources, options.Components, &recipe)
	if err != nil {
		return bale.CreateResult{}, err
	}
	name := ref.Registry + "/" + ref.Repository
	if err := sources.addAuthoredComponent(ctx, name, ref.Reference, store, root, policies); err != nil {
		return bale.CreateResult{}, err
	}
	// A complete payload capture can deliberately omit discovery. Compare Bale's
	// proposed observations before it exposes output: omitted discovery must not
	// become a new assertion that a component has no dependencies. This is a
	// metadata-only preflight over pinned local sources, not a second payload copy.
	planned, err := client.CreateLayout(ctx, recipe, options.Output, bale.CreateOptions{DryRun: true})
	if err != nil {
		return bale.CreateResult{}, err
	}
	for _, observed := range planned.Metadata.Dependencies {
		if sources.authored[bale.ArtifactID{Repository: observed.Repository, Digest: observed.Subject.Digest}] {
			continue // The newly authored parent declaration was prepared above.
		}
		input := sources.repositories[observed.Repository]
		if input == nil || !slices.ContainsFunc(input.observations, func(previous bale.DependencyResolution) bool {
			return reflect.DeepEqual(previous, observed)
		}) {
			return bale.CreateResult{}, fmt.Errorf("dependency discovery for %s@%s under policy %+v is not present in complete input bundles", observed.Repository, observed.Subject.Digest, observed.Policy)
		}
	}
	return client.CreateLayout(ctx, recipe, options.Output, bale.CreateOptions{})
}

// Only the upstream registry-source seam is adapted. Bale still validates
// declarations, follows dependencies, verifies bytes and writes the bundle.
type componentBundleSources struct {
	repositories map[string]*componentBundleRepository
	authored     map[bale.ArtifactID]bool
	captures     []componentBundleCapture
}

type componentBundleCapture struct {
	path string
	root v1.Descriptor
	plan *bale.DeliveryPlan
}

func newComponentBundleSources() *componentBundleSources {
	return &componentBundleSources{repositories: map[string]*componentBundleRepository{}, authored: map[bale.ArtifactID]bool{}}
}

func (s *componentBundleSources) open(name string) *componentBundleRepository {
	if s.repositories[name] == nil {
		s.repositories[name] = &componentBundleRepository{selectors: map[string]v1.Descriptor{}, referrers: map[digest.Digest][]v1.Descriptor{}, aliasSubjects: map[digest.Digest]*componentBundleRepository{}, aliasSeen: map[digest.Digest]bool{}}
	}
	return s.repositories[name]
}

func (s *componentBundleSources) repository(ctx context.Context, name string) (bale.RegistryRepository, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if repo := s.repositories[name]; repo != nil {
		return repo, nil
	}
	return nil, fmt.Errorf("dependency repository %q is absent from complete input bundles: %w", name, errdef.ErrNotFound)
}

func (s *componentBundleSources) addLayout(ctx context.Context, client *bale.Client, path string) error {
	report, err := client.InspectLayout(ctx, path)
	if err != nil {
		return err
	}
	if report.MetadataOnly {
		return errors.New("metadata-only capture is not a complete dependency bundle; materialize it with Bale first")
	}
	store, err := orasoci.NewFromFS(ctx, os.DirFS(path))
	if err != nil {
		return err
	}
	s.captures = append(s.captures, componentBundleCapture{path: path, root: report.Descriptor})
	queue := []bale.InspectResult{report}
	seen := map[digest.Digest]bool{report.Descriptor.Digest: true}
	for len(queue) > 0 {
		captured := queue[0]
		queue = queue[1:]
		for _, origin := range captured.Metadata.Origins {
			repo := s.open(origin.Repository)
			repo.addStore(store)
			if err := repo.addSelector(origin.Descriptor.Digest.String(), origin.Descriptor); err != nil {
				return err
			}
			if seen[origin.Descriptor.Digest] || origin.Descriptor.MediaType != v1.MediaTypeImageIndex {
				continue
			}
			raw, err := fetchMetadata(ctx, store, origin.Descriptor)
			if err != nil {
				return err
			}
			var index v1.Index
			if err := json.Unmarshal(raw, &index); err != nil {
				return err
			}
			if index.ArtifactType == bale.BundleArtifactType {
				nested, err := client.InspectRoot(ctx, bale.Root{Descriptor: origin.Descriptor, Store: store})
				if err != nil {
					return err
				}
				seen[origin.Descriptor.Digest] = true
				queue = append(queue, nested)
			}
		}
		for _, root := range captured.Metadata.Roots {
			if err := s.open(root.Repository).addSelector(root.Reference, root.Descriptor); err != nil {
				return err
			}
		}
		for _, resolution := range captured.Metadata.Dependencies {
			if err := s.open(resolution.Repository).addObservation(resolution); err != nil {
				return err
			}
			for _, dependency := range resolution.Dependencies {
				ref, err := registry.ParseReference(dependency.Ref)
				if err != nil {
					return err
				}
				if err := s.open(ref.Registry+"/"+ref.Repository).addSelector(ref.Reference, dependency.Descriptor); err != nil {
					return err
				}
			}
		}
		for _, attachment := range captured.Metadata.Referrers {
			s.open(attachment.Repository).addReferrer(attachment.Subject, attachment.Descriptor)
		}
	}
	return nil
}

type componentBundleRepository struct {
	stores        []content.ReadOnlyStorage
	resolvers     []content.Resolver
	selectors     map[string]v1.Descriptor
	referrers     map[digest.Digest][]v1.Descriptor
	observations  []bale.DependencyResolution
	aliasSources  []*componentBundleRepository
	aliasSubjects map[digest.Digest]*componentBundleRepository
	aliasName     string
	aliasSeen     map[digest.Digest]bool
}

func (r *componentBundleRepository) addStore(store *orasoci.ReadOnlyStore) {
	for _, existing := range r.stores {
		if existing == store {
			return
		}
	}
	r.stores = append(r.stores, store)
	r.resolvers = append(r.resolvers, store)
}

func (r *componentBundleRepository) addSelector(selector string, descriptor v1.Descriptor) error {
	if previous, ok := r.selectors[selector]; ok && !content.Equal(previous, descriptor) {
		return fmt.Errorf("input bundles lock conflicting results for selector %q", selector)
	}
	r.selectors[selector] = descriptor
	return nil
}

func (r *componentBundleRepository) addReferrer(subject, attachment v1.Descriptor) {
	if !slices.ContainsFunc(r.referrers[subject.Digest], func(d v1.Descriptor) bool { return content.Equal(d, attachment) }) {
		r.referrers[subject.Digest] = append(r.referrers[subject.Digest], attachment)
	}
}

func (r *componentBundleRepository) Fetch(ctx context.Context, descriptor v1.Descriptor) (io.ReadCloser, error) {
	for _, store := range r.stores {
		exists, err := store.Exists(ctx, descriptor)
		if err != nil {
			return nil, err
		}
		if exists {
			return store.Fetch(ctx, descriptor)
		}
	}
	return nil, errdef.ErrNotFound
}

func (r *componentBundleRepository) Exists(ctx context.Context, descriptor v1.Descriptor) (bool, error) {
	for _, store := range r.stores {
		exists, err := store.Exists(ctx, descriptor)
		if exists || err != nil {
			return exists, err
		}
	}
	return false, ctx.Err()
}

func (r *componentBundleRepository) Resolve(ctx context.Context, selector string) (v1.Descriptor, error) {
	if err := ctx.Err(); err != nil {
		return v1.Descriptor{}, err
	}
	if descriptor, ok := r.selectors[selector]; ok {
		return descriptor, nil
	}
	return v1.Descriptor{}, fmt.Errorf("selector %q was not captured: %w", selector, errdef.ErrNotFound)
}

// OCI layouts provide actual blob size/digest resolution independently of the
// repository's captured manifest selectors, matching Bale's split interface.
type componentBundleBlobs struct{ *componentBundleRepository }

func (r componentBundleBlobs) Resolve(ctx context.Context, selector string) (v1.Descriptor, error) {
	if err := ctx.Err(); err != nil {
		return v1.Descriptor{}, err
	}
	if descriptor, ok := r.selectors[selector]; ok {
		return descriptor, nil
	}
	if digest.Digest(selector).Validate() != nil {
		return v1.Descriptor{}, errdef.ErrNotFound
	}
	for _, resolver := range r.resolvers {
		descriptor, err := resolver.Resolve(ctx, selector)
		if err == nil || !errors.Is(err, errdef.ErrNotFound) {
			return descriptor, err
		}
	}
	return v1.Descriptor{}, fmt.Errorf("selector %q was not captured: %w", selector, errdef.ErrNotFound)
}

func (r *componentBundleRepository) Push(context.Context, v1.Descriptor, io.Reader) error {
	return errdef.ErrUnsupported
}
func (r *componentBundleRepository) Tag(context.Context, v1.Descriptor, string) error {
	return errdef.ErrUnsupported
}
func (r *componentBundleRepository) Blobs() bale.RegistryBlobStore { return componentBundleBlobs{r} }
func (r *componentBundleRepository) ReferrerPages(ctx context.Context, subject v1.Descriptor, fn func(bale.ReferrerPage) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := r.replayAliasSubject(subject); err != nil {
		return err
	}
	if known, ok := r.selectors[subject.Digest.String()]; !ok || !content.Equal(known, subject) {
		return fmt.Errorf("referrer subject was not captured: %w", errdef.ErrNotFound)
	}
	data, err := json.Marshal(v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageIndex, Manifests: append([]v1.Descriptor{}, r.referrers[subject.Digest]...)})
	if err != nil {
		return err
	}
	return fn(bale.ReferrerPage{MediaType: v1.MediaTypeImageIndex, Size: int64(len(data)), Digest: digest.FromBytes(data), Content: bytes.NewReader(data)})
}

func (r *componentBundleRepository) addObservation(observed bale.DependencyResolution) error {
	for _, previous := range r.observations {
		if content.Equal(previous.Subject, observed.Subject) && reflect.DeepEqual(previous.Policy, observed.Policy) {
			if !reflect.DeepEqual(previous, observed) {
				return fmt.Errorf("input bundles contain conflicting dependency observations for %s@%s", observed.Repository, observed.Subject.Digest)
			}
			return nil
		}
	}
	r.observations = append(r.observations, observed)
	return nil
}

// Bale supplies the actually traversed subjects. Explicit roots select their
// original repository; shared structural members must have compatible captured
// observations. This does not copy a repository's unrelated selector namespace.
func (r *componentBundleRepository) replayAliasSubject(subject v1.Descriptor) error {
	if len(r.aliasSources) == 0 || r.aliasSeen[subject.Digest] {
		return nil
	}
	sources := r.aliasSources
	if selected, explicit := r.aliasSubjects[subject.Digest]; explicit {
		if selected == nil {
			return nil
		} // Newly authored/provenance subject.
		sources = []*componentBundleRepository{selected}
	}
	found := false
	for _, source := range sources {
		known, ok := source.selectors[subject.Digest.String()]
		if !ok || !content.Equal(known, subject) {
			continue
		}
		found = true
		if err := r.addSelector(subject.Digest.String(), known); err != nil {
			return err
		}
		for _, attachment := range source.referrers[subject.Digest] {
			if err := r.addSelector(attachment.Digest.String(), attachment); err != nil {
				return err
			}
			r.addReferrer(subject, attachment)
		}
		for _, observed := range source.observations {
			if content.Equal(observed.Subject, subject) {
				observed.Repository = r.aliasName
				if err := r.addObservation(observed); err != nil {
					return err
				}
			}
		}
	}
	if !found {
		return fmt.Errorf("alias subject was not captured: %w", errdef.ErrNotFound)
	}
	r.aliasSeen[subject.Digest] = true
	return nil
}
