package oci

import (
	"fmt"

	"oras.land/oras-go/v2"
)

// CacheRepository returns an authenticated OCI target for a runtime-selected
// repository. The cache format is owned by its caller, not by the resolver.
func (r *Resolver) CacheRepository(name string) (oras.Target, error) {
	if r == nil {
		return nil, fmt.Errorf("nil OCI resolver")
	}
	ref, err := ParseReference(name + ":coopr-cache")
	if err != nil || ref.Registry+"/"+ref.Repository != name {
		return nil, fmt.Errorf("invalid cache repository %q: %v", name, err)
	}
	return r.repository(ref)
}
