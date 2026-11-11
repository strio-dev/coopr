package oci

import (
	"context"
	"fmt"

	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/registry"
)

// CacheRepository returns an authenticated OCI target for a runtime-selected
// repository. The cache format is owned by its caller, not by the resolver.
func (r *Resolver) CacheRepository(name string) (oras.Target, error) {
	if r == nil {
		return nil, fmt.Errorf("nil OCI resolver")
	}
	ref, err := cacheRepositoryReference(name)
	if err != nil {
		return nil, err
	}
	return r.repository(ref)
}

// PublishCacheGraph copies a complete cache artifact through Bale's role-aware
// registry transport, then retains its tag only after closure verification.
// An image manifest used as an artifact layer must exist as both a manifest and
// a blob; ordinary ORAS graph copying alone does not register that second role.
func (r *Resolver) PublishCacheGraph(ctx context.Context, name string, source content.ReadOnlyStorage, root v1.Descriptor, tag string) error {
	target, err := r.CacheRepository(name)
	if err != nil {
		return err
	}
	ref, err := cacheRepositoryReference(name)
	if err != nil {
		return err
	}
	client, err := r.baleClient(ctx, name+"@"+root.Digest.String())
	if err != nil {
		return err
	}
	plan, err := client.Plan(ctx, bale.TransferRequest{
		Roots: []bale.Root{{
			Repository: name,
			Descriptor: root,
			Store:      source,
			Referrers:  bale.ReferrerPolicy{Include: "none"},
		}},
		Destination: ref.Registry,
	})
	if err != nil {
		return fmt.Errorf("plan cache graph publication: %w", err)
	}
	if _, err := client.Execute(ctx, plan); err != nil {
		return fmt.Errorf("publish cache graph: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return target.Tag(ctx, root, tag)
}

func cacheRepositoryReference(name string) (registry.Reference, error) {
	ref, err := ParseReference(name + ":coopr-cache")
	if err != nil || ref.Registry+"/"+ref.Repository != name {
		return registry.Reference{}, fmt.Errorf("invalid cache repository %q: %v", name, err)
	}
	return ref, nil
}
