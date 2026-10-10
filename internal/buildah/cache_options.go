package buildah

import (
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"time"

	"go.podman.io/buildah/pkg/parse"
)

type CacheSpec struct {
	Transport string `json:"transport"`
	Reference string `json:"reference"`
}

func validateCacheTTL(ttl *time.Duration) error {
	if ttl != nil && *ttl < 0 {
		return errors.New("cache TTL must not be negative")
	}
	return nil
}

func sameCacheStore(a, b any) bool {
	left, right := reflect.ValueOf(a), reflect.ValueOf(b)
	if !left.IsValid() || !right.IsValid() || left.Type() != right.Type() {
		return false
	}
	if left.Type().Comparable() {
		return left.Interface() == right.Interface()
	}
	return false
}

func normalizeCacheSpec(spec CacheSpec) (CacheSpec, error) {
	switch spec.Transport {
	case "oci-layout":
		if spec.Reference == "" {
			return CacheSpec{}, errors.New("cache OCI layout path is empty")
		}
		if !filepath.IsAbs(spec.Reference) {
			return CacheSpec{}, errors.New("cache OCI layout path must be absolute")
		}
		spec.Reference = filepath.Clean(spec.Reference)
	case "registry":
		repositories, err := parse.RepoNamesToNamedReferences([]string{spec.Reference})
		if err != nil {
			return CacheSpec{}, err
		}
		spec.Reference = repositories[0].Name()
	default:
		return CacheSpec{}, fmt.Errorf("unsupported cache transport %q", spec.Transport)
	}
	return spec, nil
}

type cacheBinding struct {
	spec        CacheSpec
	read, write bool
}

func cacheBindings(options PlanOptions) ([]cacheBinding, error) {
	bindings := map[string]cacheBinding{}
	add := func(spec CacheSpec, read, write bool) error {
		normalized, err := normalizeCacheSpec(spec)
		if err != nil {
			return err
		}
		key := normalized.Transport + ":" + normalized.Reference
		binding := bindings[key]
		binding.spec = normalized
		binding.read = binding.read || read
		binding.write = binding.write || write
		bindings[key] = binding
		return nil
	}
	if options.CacheLocalDir != "" {
		if err := add(CacheSpec{Transport: "oci-layout", Reference: options.CacheLocalDir}, true, true); err != nil {
			return nil, err
		}
	}
	if options.CacheRepository != "" {
		if err := add(CacheSpec{Transport: "registry", Reference: options.CacheRepository}, true, true); err != nil {
			return nil, err
		}
	}
	for _, spec := range options.CacheFrom {
		if err := add(spec, true, false); err != nil {
			return nil, err
		}
	}
	for _, spec := range options.CacheTo {
		if err := add(spec, false, true); err != nil {
			return nil, err
		}
	}
	result := make([]cacheBinding, 0, len(bindings))
	for _, binding := range bindings {
		result = append(result, binding)
	}
	// Local first keeps registry hits able to seed an explicitly writable local cache.
	slices.SortFunc(result, func(a, b cacheBinding) int {
		return strings.Compare(a.spec.Transport+":"+a.spec.Reference, b.spec.Transport+":"+b.spec.Reference)
	})
	return result, nil
}

func cacheArtifactPaths(options PlanOptions) ([]string, error) {
	bindings, err := cacheBindings(options)
	if err != nil {
		return nil, err
	}
	var result []string
	for _, binding := range bindings {
		if binding.spec.Transport == "oci-layout" {
			result = append(result, binding.spec.Reference)
		}
	}
	return result, nil
}
