package oci

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"reflect"
	"strings"
	"time"

	"coopr/internal/componentstore"

	dockerreference "github.com/distribution/reference"
	"github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale"
	"go.podman.io/image/v5/transports"
	"go.podman.io/image/v5/transports/alltransports"
	"oras.land/oras-go/v2/content"
	orasoci "oras.land/oras-go/v2/content/oci"
	"oras.land/oras-go/v2/errdef"
	"oras.land/oras-go/v2/registry/remote"
	orasretry "oras.land/oras-go/v2/registry/remote/retry"
)

// DeliveryScope selects one resolution within a delivered bundle. Dependencies
// carry their own policy, so a nested component must not inherit its parent's policy.
type DeliveryScope struct {
	Bundle  string                `json:"bundle"`
	Capture digest.Digest         `json:"capture"`
	Policy  bale.DependencyPolicy `json:"policy"`
}

// ComponentDelivery is a verified, frozen selection from an original Bale
// attachment. It is build-session state, never part of a component's payload.
type ComponentDelivery struct {
	Repository string                     `json:"repository"`
	Subject    v1.Descriptor              `json:"subject"`
	Attachment v1.Descriptor              `json:"attachment"`
	Scope      DeliveryScope              `json:"scope"`
	Resolution bale.DestinationResolution `json:"resolution"`
}

// ComponentSelection preserves root-index identity separately from its selected
// platform and freezes destination metadata before execution in another process.
type ComponentSelection struct {
	Repository string             `json:"repository"`
	Root       v1.Descriptor      `json:"root"`
	Selected   v1.Descriptor      `json:"selected"`
	Local      bool               `json:"local,omitempty"`
	Delivery   *ComponentDelivery `json:"delivery,omitempty"`
}

func cloneDeliveryValue[T any](value T) T {
	// These structs contain only JSON-native fields and OCI digest strings.
	raw, _ := json.Marshal(value)
	var result T
	_ = json.Unmarshal(raw, &result)
	return result
}

func (s *DeliveryScope) Clone() *DeliveryScope         { return cloneDeliveryValue(s) }
func (d *ComponentDelivery) Clone() *ComponentDelivery { return cloneDeliveryValue(d) }
func (s ComponentSelection) Clone() ComponentSelection { return cloneDeliveryValue(s) }

func (s ComponentSelection) Identity() digest.Digest {
	raw, _ := json.Marshal(s)
	return digest.FromBytes(raw)
}

func (d *ComponentDelivery) Identity() digest.Digest {
	if d == nil {
		return ""
	}
	canonical := d.Clone()
	canonical.Subject.Annotations = nil
	canonical.Subject.URLs, canonical.Subject.Data = nil, nil
	canonical.Attachment.Annotations = nil
	canonical.Attachment.URLs, canonical.Attachment.Data = nil, nil
	raw, _ := json.Marshal(canonical)
	return digest.FromBytes(raw)
}

func (r *Resolved) ComponentSelection() ComponentSelection {
	return (ComponentSelection{Repository: r.Repository, Root: r.Root, Selected: r.Selected, Local: r.Local, Delivery: r.Delivery}).Clone()
}

// registryInput uses the same canonical spelling as publication-time dependency
// discovery. Other transports and explicit local component paths stay untouched.
func registryInput(kind, value string) (string, bool) {
	if strings.HasPrefix(value, "./") || strings.HasPrefix(value, "../") || strings.HasPrefix(value, "/") || digest.Digest(value).Validate() == nil {
		return "", false
	}
	if prefix, _, ok := strings.Cut(value, ":"); kind == "image" && ok && transports.Get(prefix) != nil {
		if prefix != "docker" {
			return "", false
		}
		ref, err := alltransports.ParseImageName(value)
		if err != nil || ref.DockerReference() == nil {
			return "", false
		}
		value = ref.DockerReference().String()
	}
	ref, err := dockerreference.ParseNormalizedNamed(value)
	if err != nil {
		return "", false
	}
	if pinned, ok := ref.(dockerreference.Canonical); ok {
		return dockerreference.TrimNamed(ref).String() + "@" + pinned.Digest().String(), true
	}
	return dockerreference.TagNameOnly(ref).String(), true
}

func (d *ComponentDelivery) MapExternalInput(kind string, reference string) (string, error) {
	if d == nil {
		return reference, nil
	}
	canonical, ok := registryInput(kind, reference)
	if !ok {
		return reference, nil
	}
	result := ""
	for _, dependency := range d.Resolution.Dependencies {
		if canonical != dependency.Ref && canonical != dependency.Source && canonical != dependency.Destination {
			continue
		}
		if result != "" && result != dependency.Destination {
			return "", fmt.Errorf("ambiguous delivered dependency %q", reference)
		}
		result = dependency.Destination
	}
	if result == "" {
		return reference, nil
	}
	return result, nil
}

func (d *ComponentDelivery) DependencyScope(reference string) (*DeliveryScope, error) {
	if d == nil {
		return nil, nil
	}
	canonical, ok := registryInput("component", reference)
	if !ok {
		return nil, nil
	}
	var selected *DeliveryScope
	exact := false
	for _, dependency := range d.Resolution.Dependencies {
		if canonical == dependency.Ref {
			exact = true
			break
		}
	}
	for _, dependency := range d.Resolution.Dependencies {
		if exact && canonical != dependency.Ref {
			continue
		}
		if canonical != dependency.Ref && canonical != dependency.Source && canonical != dependency.Destination {
			continue
		}
		scope := &DeliveryScope{Bundle: d.Scope.Bundle, Capture: d.Scope.Capture, Policy: dependency.Policy}
		if selected != nil && !reflect.DeepEqual(selected, scope) {
			return nil, fmt.Errorf("ambiguous delivered dependency policy for %q", reference)
		}
		selected = scope
	}
	return selected.Clone(), nil
}

func selectComponentDelivery(records []bale.InspectedDestination, repository string, root v1.Descriptor, scope *DeliveryScope) (*ComponentDelivery, error) {
	var selected *ComponentDelivery
	for _, item := range records {
		if !item.Local || !content.Equal(item.Subject, root) || item.Record.Destination != repository+"@"+root.Digest.String() {
			continue
		}
		for _, resolution := range item.Record.Resolutions {
			candidateScope := DeliveryScope{Bundle: item.Record.Bundle, Capture: resolution.Capture, Policy: resolution.Policy}
			if scope != nil && !reflect.DeepEqual(*scope, candidateScope) {
				continue
			}
			candidate := &ComponentDelivery{Repository: repository, Subject: root, Attachment: item.Descriptor, Scope: candidateScope, Resolution: resolution}
			if selected != nil && selected.Identity() != candidate.Identity() {
				return nil, fmt.Errorf("component %s@%s has ambiguous Bale deliveries; select an unambiguous bundle capture", repository, root.Digest)
			}
			selected = candidate
		}
	}
	if selected == nil && scope != nil {
		return nil, fmt.Errorf("component %s@%s has no destination record for the selected bundle, capture and policy", repository, root.Digest)
	}
	return selected.Clone(), nil
}

func (r *Resolver) componentBinding(ctx context.Context, source content.ReadOnlyStorage, root v1.Descriptor, repository string) (*componentstore.Binding, error) {
	client, inspectionRepository, err := r.componentDeliveryClient(ctx, source, root, repository)
	if err != nil {
		return nil, err
	}
	records, err := client.InspectDestinationsWithOptions(ctx, inspectionRepository+"@"+root.Digest.String(), deliveryInspectionOptions())
	if err != nil {
		return nil, fmt.Errorf("inspect component delivery: %w", err)
	}
	binding := &componentstore.Binding{Repository: repository}
	for _, item := range records {
		if item.Record.Destination == repository+"@"+root.Digest.String() || item.Local {
			actualRepository, _, _ := strings.Cut(item.Record.Destination, "@")
			if len(binding.Attachments) != 0 && binding.Repository != actualRepository {
				return nil, errors.New("component has ambiguous delivery repositories")
			}
			binding.Repository = actualRepository
			binding.Attachments = append(binding.Attachments, item.Descriptor)
		}
	}
	if len(binding.Attachments) == 0 {
		return nil, nil
	}
	return binding, nil
}

// Use the authenticated source that supplied the root. Reopening the logical
// primary here would bypass mirrors, remaps and namespace proxies during reads.
type repositoryReadTransport struct{ client remote.Client }

func (t repositoryReadTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return t.client.Do(request)
}

func (r *Resolver) componentDeliveryClient(ctx context.Context, source content.ReadOnlyStorage, root v1.Descriptor, repository string) (*bale.Client, string, error) {
	if graph, ok := source.(*orasArtifactRepository); ok {
		graph.mu.Lock()
		selected := graph.sources[root.Digest].repository
		graph.mu.Unlock()
		if selected != nil {
			ref := selected.Reference
			ref.Reference = ""
			var base http.RoundTripper = repositoryReadTransport{client: selected.Client}
			base = orasMetadataTransport{base: base, repository: ref}
			base = &orasretry.Transport{Base: base, Policy: func() orasretry.Policy {
				backoff := orasretry.ExponentialBackoff(time.Second, 2, 0.1)
				if r.retryOptions.Delay != 0 {
					backoff = func(int, *http.Response) time.Duration { return r.retryOptions.Delay }
				}
				return &orasretry.GenericPolicy{MaxRetry: r.retryOptions.MaxRetry, Retryable: func(response *http.Response, err error) (bool, error) {
					// ORAS must see an unsupported Referrers API immediately so it
					// can use the standard fallback index; retrying cannot add the API.
					if err == nil && response != nil && (response.StatusCode == http.StatusNotFound || response.StatusCode == http.StatusMethodNotAllowed) && response.Request != nil && strings.HasPrefix(response.Request.URL.Path, "/v2/"+ref.Repository+"/referrers/") {
						return false, nil
					}
					return orasHTTPRetryable(response, err)
				}, Backoff: backoff, MaxWait: time.Duration(1<<63 - 1)}
			}}
			options := bale.ClientOptions{HTTPClient: &http.Client{Transport: orasReplayTransport{base: base}}}
			if selected.PlainHTTP {
				options.PlainHTTPRegistries = []string{ref.Registry}
			}
			client, err := bale.NewClient(options)
			return client, ref.Registry + "/" + ref.Repository, err
		}
	}
	client, err := r.baleClient(ctx, repository+"@"+root.Digest.String())
	return client, repository, err
}

func deliveryInspectionOptions() bale.DestinationInspectionOptions {
	return bale.DestinationInspectionOptions{MaxMetadataBytes: 64 << 20, MaxReferrers: 1024}
}

// retainedDestinationRepository exposes immutable local evidence through Bale's
// public backend seam. Bale retains ownership of envelope and record validation.
type retainedDestinationRepository struct {
	content.ReadOnlyStorage
	root        v1.Descriptor
	attachments []v1.Descriptor
}

func (r retainedDestinationRepository) Resolve(ctx context.Context, selector string) (v1.Descriptor, error) {
	if err := ctx.Err(); err != nil {
		return v1.Descriptor{}, err
	}
	if selector == r.root.Digest.String() {
		return r.root, nil
	}
	for _, attachment := range r.attachments {
		if selector == attachment.Digest.String() {
			return attachment, nil
		}
	}
	return v1.Descriptor{}, fmt.Errorf("retained component selector %q: %w", selector, errdef.ErrNotFound)
}
func (r retainedDestinationRepository) Push(context.Context, v1.Descriptor, io.Reader) error {
	return errdef.ErrUnsupported
}
func (r retainedDestinationRepository) Tag(context.Context, v1.Descriptor, string) error {
	return errdef.ErrUnsupported
}
func (r retainedDestinationRepository) Blobs() bale.RegistryBlobStore { return r }
func (r retainedDestinationRepository) ReferrerPages(ctx context.Context, subject v1.Descriptor, fn func(bale.ReferrerPage) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !content.Equal(subject, r.root) {
		return fmt.Errorf("retained destination subject differs from component root")
	}
	data, err := json.Marshal(v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageIndex, Manifests: r.attachments})
	if err != nil {
		return err
	}
	return fn(bale.ReferrerPage{MediaType: v1.MediaTypeImageIndex, Size: int64(len(data)), Digest: digest.FromBytes(data), Content: bytes.NewReader(data)})
}

func inspectRetainedDestinations(ctx context.Context, source content.ReadOnlyStorage, root v1.Descriptor, binding *componentstore.Binding) ([]bale.InspectedDestination, error) {
	if binding == nil {
		return nil, nil
	}
	repository := retainedDestinationRepository{ReadOnlyStorage: source, root: root, attachments: binding.Attachments}
	client, err := bale.NewClient(bale.ClientOptions{RepositoryFactory: func(_ context.Context, name string) (bale.RegistryRepository, error) {
		if name != binding.Repository {
			return nil, fmt.Errorf("retained destination repository differs from binding")
		}
		return repository, nil
	}})
	if err != nil {
		return nil, err
	}
	records, err := client.InspectDestinationsWithOptions(ctx, binding.Repository+"@"+root.Digest.String(), deliveryInspectionOptions())
	if err != nil {
		return nil, err
	}
	if len(records) == 0 || len(records) != len(binding.Attachments) {
		return nil, errors.New("component binding does not contain exactly its original destination attachments")
	}
	for _, record := range records {
		if !record.Local || !content.Equal(record.Subject, root) {
			return nil, errors.New("component binding destination record differs from its repository or root")
		}
		found := false
		for _, attachment := range binding.Attachments {
			if content.Equal(record.Descriptor, attachment) {
				found = true
				break
			}
		}
		if !found {
			return nil, errors.New("component binding contains an unexpected destination attachment")
		}
	}
	return records, nil
}

func (r *Resolver) componentBindingFromArchive(ctx context.Context, source content.ReadOnlyStorage, root v1.Descriptor, _ []v1.Descriptor) (*componentstore.Binding, error) {
	binding, err := componentstore.BindingFromDescriptor(root)
	if err != nil {
		return nil, err
	}
	if _, err := inspectRetainedDestinations(ctx, source, root, binding); err != nil {
		return nil, fmt.Errorf("validate archived component delivery: %w", err)
	}
	return binding, nil
}

// ResolveComponent selects destination metadata only for a coordinator's first
// lookup. Frozen worker selections verify immutable content without rediscovery.
func (r *Resolver) ResolveComponent(ctx context.Context, reference string, platform v1.Platform, frozen *ComponentSelection, scope *DeliveryScope) (*Resolved, error) {
	if frozen != nil {
		selection := frozen.Clone()
		var source content.ReadOnlyStorage
		if selection.Local {
			store, err := orasoci.NewFromFS(ctx, os.DirFS(r.componentStoreDir))
			if err != nil {
				return nil, err
			}
			source = store
		} else {
			ref, err := ParseReference(selection.Repository + "@" + selection.Root.Digest.String())
			if err != nil {
				return nil, err
			}
			repo, err := r.repository(ref)
			if err != nil {
				return nil, err
			}
			source = repo
		}
		resolved, err := r.resolveRoot(ctx, source, reference, selection.Repository, selection.Root, platform, Component)
		if err != nil {
			return nil, err
		}
		if !content.Equal(resolved.Selected, selection.Selected) {
			return nil, errors.New("frozen component platform selection changed")
		}
		resolved.Local, resolved.Delivery = selection.Local, selection.Delivery.Clone()
		if scope != nil && (resolved.Delivery == nil || !reflect.DeepEqual(*scope, resolved.Delivery.Scope)) {
			return nil, errors.New("frozen component delivery scope differs")
		}
		return resolved, nil
	}
	var selectBinding func(v1.Descriptor, *componentstore.Binding) (string, error)
	if scope != nil {
		selectBinding = func(root v1.Descriptor, binding *componentstore.Binding) (string, error) {
			if binding == nil {
				return "", nil
			}
			source, err := orasoci.NewFromFS(ctx, os.DirFS(r.componentStoreDir))
			if err != nil {
				return "", err
			}
			records, err := inspectRetainedDestinations(ctx, source, root, binding)
			if err != nil {
				return "", err
			}
			// The same original attachment may occur in several stored
			// snapshots. Deduplicate by the selected delivery, not by the
			// surrounding set of unrelated historical records.
			var matching []bale.InspectedDestination
			for _, record := range records {
				for _, resolution := range record.Record.Resolutions {
					if reflect.DeepEqual(*scope, DeliveryScope{Bundle: record.Record.Bundle, Capture: resolution.Capture, Policy: resolution.Policy}) {
						matching = append(matching, record)
						break
					}
				}
			}
			if len(matching) == 0 {
				return "", nil
			}
			delivery, err := selectComponentDelivery(matching, binding.Repository, root, scope)
			if err != nil {
				return "", err
			}
			return delivery.Identity().String(), nil
		}
	}
	store, root, binding, err := componentstore.ResolveWithBindingSelector(ctx, r.componentStoreDir, reference, selectBinding)
	if err == nil {
		repository := "local"
		if binding != nil {
			repository = binding.Repository
		}
		resolved, err := r.resolveRoot(ctx, store, reference, repository, root, platform, Component)
		if err != nil {
			return nil, err
		}
		resolved.Local = true
		records, err := inspectRetainedDestinations(ctx, store, root, binding)
		if err != nil {
			return nil, err
		}
		resolved.Delivery, err = selectComponentDelivery(records, repository, root, scope)
		return resolved, err
	}
	if !errors.Is(err, errdef.ErrNotFound) || strings.HasPrefix(reference, "sha256:") {
		return nil, fmt.Errorf("resolve component %s: %w", reference, err)
	}
	resolved, err := r.resolveRemote(ctx, reference, platform, Component)
	if err != nil {
		return nil, err
	}
	binding, err = r.componentBinding(ctx, resolved.Source(), resolved.Root, resolved.Repository)
	if err != nil {
		return nil, err
	}
	records, err := inspectRetainedDestinations(ctx, resolved.Source(), resolved.Root, binding)
	if err != nil {
		return nil, err
	}
	deliveryRepository := resolved.Repository
	if binding != nil {
		deliveryRepository = binding.Repository
	}
	resolved.Delivery, err = selectComponentDelivery(records, deliveryRepository, resolved.Root, scope)
	return resolved, err
}
