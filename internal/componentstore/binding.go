package componentstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/distribution/reference"
	digest "github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/coopr/internal/localstore"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content"
	orasoci "oras.land/oras-go/v2/content/oci"
	"oras.land/oras-go/v2/errdef"
)

// Binding records where an unchanged component was delivered, together with
// the original Bale metadata needed to interpret that delivery offline.
// It belongs to a local name, never to the component's manifest or config.
type Binding struct {
	Repository  string          `json:"repository"`
	Attachments []v1.Descriptor `json:"attachments"`
}

const bindingAnnotation = "dev.coopr.strio.component.binding"
const bindingHistoryAnnotation = "dev.coopr.strio.component.binding-history"
const archiveRepositoryAnnotation = "dev.coopr.strio.component.repository"

func normalizeBinding(binding *Binding) (*Binding, error) {
	if binding == nil {
		return nil, nil
	}
	named, err := reference.ParseNormalizedNamed(binding.Repository)
	if err != nil || reference.TrimNamed(named).String() != binding.Repository {
		return nil, fmt.Errorf("invalid component binding repository %q", binding.Repository)
	}
	result := &Binding{Repository: binding.Repository, Attachments: slices.Clone(binding.Attachments)}
	slices.SortFunc(result.Attachments, func(a, b v1.Descriptor) int { return strings.Compare(a.Digest.String(), b.Digest.String()) })
	for i, attachment := range result.Attachments {
		if attachment.Digest.Validate() != nil || attachment.Size < 0 || attachment.MediaType != v1.MediaTypeImageManifest {
			return nil, fmt.Errorf("invalid component binding attachment %q", attachment.Digest)
		}
		if i > 0 && result.Attachments[i-1].Digest == attachment.Digest {
			return nil, fmt.Errorf("duplicate component binding attachment %s", attachment.Digest)
		}
	}
	return result, nil
}

func BindingFromDescriptor(root v1.Descriptor) (*Binding, error) {
	data := root.Annotations[bindingAnnotation]
	if data == "" {
		return nil, nil
	}
	var binding Binding
	if err := json.Unmarshal([]byte(data), &binding); err != nil {
		return nil, fmt.Errorf("decode component binding: %w", err)
	}
	return normalizeBinding(&binding)
}

func withBinding(root v1.Descriptor, binding *Binding) (v1.Descriptor, error) {
	root.Annotations = maps.Clone(root.Annotations)
	if root.Annotations == nil {
		root.Annotations = make(map[string]string)
	}
	delete(root.Annotations, bindingAnnotation)
	delete(root.Annotations, bindingHistoryAnnotation)
	delete(root.Annotations, localstore.AuxiliaryRootOwner)
	if binding != nil {
		data, err := json.Marshal(binding)
		if err != nil {
			return root, err
		}
		root.Annotations[bindingAnnotation] = string(data)
	}
	return root, nil
}

func readIndex(rootDir string) (v1.Index, error) {
	data, err := os.ReadFile(filepath.Join(rootDir, v1.ImageIndexFile))
	if err != nil {
		return v1.Index{}, err
	}
	var index v1.Index
	if err := json.Unmarshal(data, &index); err != nil {
		return index, fmt.Errorf("decode component index: %w", err)
	}
	if index.SchemaVersion != 2 || index.MediaType != "" && index.MediaType != v1.MediaTypeImageIndex {
		return index, errors.New("invalid component OCI index")
	}
	return index, nil
}

func bindingKey(binding *Binding) string { data, _ := json.Marshal(binding); return string(data) }

func descriptorBindings(root v1.Descriptor) ([]*Binding, error) {
	if data := root.Annotations[bindingHistoryAnnotation]; data != "" {
		var bindings []*Binding
		if err := json.Unmarshal([]byte(data), &bindings); err != nil {
			return nil, fmt.Errorf("decode component binding history: %w", err)
		}
		for i, binding := range bindings {
			normalized, err := normalizeBinding(binding)
			if err != nil {
				return nil, err
			}
			bindings[i] = normalized
		}
		return bindings, nil
	}
	binding, err := BindingFromDescriptor(root)
	return []*Binding{binding}, err
}

// PutWithBinding copies all content before atomically publishing the local
// name and its delivery context under the same OCI-store lock.
func PutWithBinding(ctx context.Context, rootDir string, source content.ReadOnlyStorage, root v1.Descriptor, tag string, binding *Binding) error {
	if source == nil {
		return errors.New("nil component graph source")
	}
	normalizedTag, err := NormalizeTag(tag)
	if err != nil {
		return err
	}
	binding, err = normalizeBinding(binding)
	if err != nil {
		return err
	}
	root, err = withBinding(root, binding)
	if err != nil {
		return err
	}
	return localstore.Update(ctx, rootDir, func(target *orasoci.Store) error {
		index, err := readIndex(rootDir)
		if err != nil {

			return err
		}
		history := map[string]*Binding{bindingKey(binding): binding}
		for _, entry := range index.Manifests {
			if entry.Digest != root.Digest || entry.Annotations[localstore.AuxiliaryRootOwner] != "" {
				continue
			}
			previous, err := descriptorBindings(entry)
			if err != nil {
				return err
			}
			for _, old := range previous {
				history[bindingKey(old)] = old
			}
		}
		if err := oras.CopyGraph(ctx, source, target, root, oras.CopyGraphOptions{}); err != nil {
			return fmt.Errorf("copy component graph: %w", err)
		}
		if binding != nil {
			for _, attachment := range binding.Attachments {
				if err := oras.CopyGraph(ctx, source, target, attachment, oras.CopyGraphOptions{}); err != nil {
					return fmt.Errorf("copy component delivery attachment: %w", err)
				}
				attachment.Annotations = maps.Clone(attachment.Annotations)
				if attachment.Annotations == nil {
					attachment.Annotations = make(map[string]string)
				}
				attachment.Annotations[localstore.AuxiliaryRootOwner] = root.Digest.String()
				delete(attachment.Annotations, v1.AnnotationRefName)
				if err := target.Tag(ctx, attachment, attachment.Digest.String()); err != nil {
					return err
				}
			}
		}
		keys := make([]string, 0, len(history))
		for key := range history {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		bindings := make([]*Binding, 0, len(keys))
		for _, key := range keys {
			bindings = append(bindings, history[key])
		}
		anchor := root
		anchor.Annotations = maps.Clone(root.Annotations)
		delete(anchor.Annotations, v1.AnnotationRefName)
		data, err := json.Marshal(bindings)
		if err != nil {
			return err
		}
		anchor.Annotations[bindingHistoryAnnotation] = string(data)
		// ORAS omits a digest anchor from index.json while names point at the
		// same digest. Preserve the immutable context history on every such
		// descriptor so moving the last name cannot discard an older delivery.
		for _, entry := range index.Manifests {
			name := entry.Annotations[v1.AnnotationRefName]
			if entry.Digest != root.Digest || name == "" || entry.Annotations[localstore.AuxiliaryRootOwner] != "" {
				continue
			}
			entry.Annotations = maps.Clone(entry.Annotations)
			entry.Annotations[bindingHistoryAnnotation] = string(data)
			if err := target.Tag(ctx, entry, name); err != nil {
				return err
			}
		}
		root.Annotations[bindingHistoryAnnotation] = string(data)
		if err := target.Tag(ctx, anchor, root.Digest.String()); err != nil {
			return err
		}
		if normalizedTag != "" {
			return target.Tag(ctx, root, normalizedTag)
		}
		return nil
	})
}

// Resolve keeps repository-qualified digest lookups distinct even when several
// deliveries contain identical component bytes. Bare digests reject ambiguity.
func Resolve(ctx context.Context, rootDir, value string) (*orasoci.ReadOnlyStore, v1.Descriptor, *Binding, error) {
	return ResolveWithBindingSelector(ctx, rootDir, value, nil)
}

// ResolveWithBindingSelector selects caller-validated immutable contexts before
// ambiguity checks. An empty key rejects a candidate; equal keys identify the
// same context even across overlapping attachment snapshots. The callback runs
// under the index read lock and must not acquire another store lock.
func ResolveWithBindingSelector(ctx context.Context, rootDir, value string, selectBinding func(v1.Descriptor, *Binding) (string, error)) (*orasoci.ReadOnlyStore, v1.Descriptor, *Binding, error) {
	normalized, err := NormalizeReference(value)
	if err != nil {
		return nil, v1.Descriptor{}, nil, err
	}
	if _, err := os.Stat(rootDir); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, v1.Descriptor{}, nil, errdef.ErrNotFound
		}
		return nil, v1.Descriptor{}, nil, err
	}
	var store *orasoci.ReadOnlyStore
	var root v1.Descriptor
	var chosen *Binding
	err = localstore.WithReadLock(ctx, rootDir, func() error {
		index, err := readIndex(rootDir)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				if _, layoutErr := os.Stat(filepath.Join(rootDir, v1.ImageLayoutFile)); errors.Is(layoutErr, os.ErrNotExist) {
					entries, readErr := os.ReadDir(rootDir)
					if readErr != nil {
						return readErr
					}
					for _, entry := range entries {
						if entry.Type().IsRegular() && (entry.Name() == ".coopr-store.lock" || entry.Name() == ".coopr-activity.lock") {
							continue
						}
						return fmt.Errorf("component store is missing OCI metadata with resident entry %q", entry.Name())
					}
					return errdef.ErrNotFound
				}
			}
			return err
		}
		selector := normalized
		repository := ""
		qualifiedName := ""
		explicitTag := false
		if at := strings.LastIndexByte(normalized, '@'); at >= 0 {
			selector = normalized[at+1:]
			named, err := reference.ParseNormalizedNamed(normalized[:at])
			if err != nil {
				return err
			}
			repository = reference.TrimNamed(named).String()
			qualifiedName = reference.TagNameOnly(named).String()
			_, explicitTag = named.(reference.Tagged)
		}
		pinned := digest.Digest(selector)
		type candidate struct {
			descriptor v1.Descriptor
			binding    *Binding
		}
		candidates := map[string]candidate{}
		currentNames := map[string]candidate{}
		selectedLocalName := false
		hasBoundHistory := false
		hasMatchingBoundRepository := false
		selectionKey := func(descriptor v1.Descriptor, binding *Binding) (string, error) {
			if selectBinding == nil {
				return bindingKey(binding), nil
			}
			descriptor, err := withBinding(descriptor, binding)
			if err != nil {
				return "", err
			}
			return selectBinding(descriptor, binding)
		}
		for _, entry := range index.Manifests {
			if entry.Annotations[localstore.AuxiliaryRootOwner] != "" {
				continue
			}
			if pinned.Validate() != nil {
				if entry.Annotations[v1.AnnotationRefName] != selector {
					continue
				}
				root = entry
				chosen, err = BindingFromDescriptor(entry)
				if err != nil {
					return err
				}
				key, err := selectionKey(entry, chosen)
				if err != nil {
					return err
				}
				if key == "" {
					return errdef.ErrNotFound
				}
				break
			}
			if entry.Digest != pinned {
				continue
			}
			if repository != "" && entry.Annotations[v1.AnnotationRefName] != "" {
				name, err := reference.ParseNormalizedNamed(entry.Annotations[v1.AnnotationRefName])
				if err != nil {
					return err
				}
				if reference.TagNameOnly(name).String() == qualifiedName {
					binding, err := BindingFromDescriptor(entry)
					if err != nil {
						return err
					}
					key, err := selectionKey(entry, binding)
					if err != nil {
						return err
					}
					if key != "" {
						currentNames[key] = candidate{descriptor: entry, binding: binding}
					}
				}
			}
			bindings, err := descriptorBindings(entry)
			if err != nil {
				return err
			}
			for _, binding := range bindings {
				if binding != nil {
					hasBoundHistory = true
					if binding.Repository == repository {
						hasMatchingBoundRepository = true
					}
				}
				if repository != "" && binding != nil && binding.Repository != repository {
					continue
				}
				key, err := selectionKey(entry, binding)
				if err != nil {
					return err
				}
				if key == "" {
					continue
				}
				candidates[key] = candidate{descriptor: entry, binding: binding}
			}
		}
		if pinned.Validate() == nil {
			if repository != "" {
				// A current local name selects its own immutable context, before
				// historical deliveries sharing the same content digest.
				if len(currentNames) != 0 && (explicitTag || !hasMatchingBoundRepository) {
					candidates = currentNames
					selectedLocalName = true
				} else if hasBoundHistory {
					// Unbound content cannot satisfy a foreign repository qualifier
					// when these bytes have recorded delivery identities.
					for key, selection := range candidates {
						if selection.binding == nil {
							delete(candidates, key)
						}
					}
				}
			}
			if len(candidates) == 0 {
				return errdef.ErrNotFound
			}
			if len(candidates) > 1 {
				return fmt.Errorf("component %s has multiple delivery contexts; use an unambiguous local name or repository-qualified digest", value)
			}
			for _, selection := range candidates {
				chosen = selection.binding
				root = selection.descriptor
			}
		}
		if root.Digest == "" {
			return errdef.ErrNotFound
		}
		root, err = withBinding(root, chosen)
		if err != nil {
			return err
		}
		if pinned.Validate() == nil && !selectedLocalName {
			delete(root.Annotations, v1.AnnotationRefName)
		}
		store, err = orasoci.NewFromFS(ctx, os.DirFS(rootDir))
		return err
	})
	return store, root, chosen, err
}
