// Package imagecatalog records which immutable storage image represents each
// locally available OCI image manifest. Image bytes live in containers/storage;
// this catalog only owns names, platform selections, and verified metadata.
package imagecatalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/containerd/platforms"
	"github.com/gofrs/flock"
	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

const filename = "catalog.json"
const version = 1
const dockerManifestMediaType = "application/vnd.docker.distribution.manifest.v2+json"
const dockerIndexMediaType = "application/vnd.docker.distribution.manifest.list.v2+json"

type Selection struct {
	Root     v1.Descriptor `json:"root"`
	Manifest v1.Descriptor `json:"manifest"`
	// Decryption changes the stored manifest while preserving the registry
	// selection used to decide whether a newer pull is necessary.
	SourceManifest *v1.Descriptor `json:"source_manifest,omitempty"`
	ImageID        string         `json:"image_id"`
	// Configuration bytes define the storage image ID. Store them as opaque
	// bytes so JSON encoding cannot compact whitespace or escape their content.
	ConfigData []byte `json:"config_data"`
}

// Entry is one locally named OCI image root and its immutable platform
// selections.
type Entry struct {
	Reference string               `json:"reference"`
	Root      v1.Descriptor        `json:"root"`
	Platforms map[string]Selection `json:"platforms"`
}

type rootRecord struct {
	Root       v1.Descriptor        `json:"root"`
	Selections map[string]Selection `json:"selections"`
	IndexData  []byte               `json:"index_data,omitempty"`
}

type catalog struct {
	Version      int                   `json:"version"`
	Refs         map[string]string     `json:"refs"`
	RefPlatforms map[string]string     `json:"ref_platforms,omitempty"`
	Roots        map[string]rootRecord `json:"roots"`
}

func platformKey(platform v1.Platform) (string, error) {
	if platform.OS == "" || platform.Architecture == "" {
		return "", errors.New("image platform requires OS and architecture")
	}
	return platforms.Format(platforms.Normalize(platform)), nil
}

func validDescriptor(d v1.Descriptor) bool {
	return d.Digest.Algorithm() == digest.SHA256 && d.Digest.Validate() == nil && d.Size >= 0 && d.MediaType != ""
}

func sameDescriptor(left, right v1.Descriptor) bool {
	return left.Digest == right.Digest && left.Size == right.Size && left.MediaType == right.MediaType
}

func indexMediaType(mediaType string) bool {
	return mediaType == v1.MediaTypeImageIndex || mediaType == dockerIndexMediaType
}

func validate(selection Selection) error {
	if !validDescriptor(selection.Root) || !validDescriptor(selection.Manifest) {
		return errors.New("image catalog has invalid OCI descriptor")
	}
	if selection.SourceManifest != nil && !validDescriptor(*selection.SourceManifest) {
		return errors.New("image catalog has invalid source manifest descriptor")
	}
	if selection.ImageID == "" || strings.ContainsAny(selection.ImageID, "/\\ \t\r\n") {
		return errors.New("image catalog has invalid storage image ID")
	}
	if !json.Valid(selection.ConfigData) || len(selection.ConfigData) == 0 {
		return errors.New("image catalog has invalid image configuration")
	}
	if digest.FromBytes(selection.ConfigData).Encoded() != selection.ImageID {
		return errors.New("image catalog storage ID does not match image configuration")
	}
	return nil
}

func validateIndex(root v1.Descriptor, indexData []byte, selections map[string]Selection) error {
	if !validDescriptor(root) || !indexMediaType(root.MediaType) {
		return errors.New("image catalog has invalid OCI index descriptor")
	}
	if int64(len(indexData)) != root.Size || digest.FromBytes(indexData) != root.Digest {
		return errors.New("image catalog index bytes do not match root descriptor")
	}
	var index v1.Index
	if err := json.Unmarshal(indexData, &index); err != nil {
		return fmt.Errorf("decode image catalog index: %w", err)
	}
	if index.SchemaVersion != 2 || index.MediaType != root.MediaType || len(index.Manifests) == 0 {
		return errors.New("image catalog has invalid OCI index")
	}
	if len(selections) != len(index.Manifests) {
		return errors.New("image catalog index selections are incomplete")
	}
	seen := make(map[string]struct{}, len(index.Manifests))
	for _, manifest := range index.Manifests {
		if !validDescriptor(manifest) || manifest.Platform == nil {
			return errors.New("image catalog index has invalid platform descriptor")
		}
		key, err := platformKey(*manifest.Platform)
		if err != nil {
			return fmt.Errorf("image catalog index platform: %w", err)
		}
		if _, duplicate := seen[key]; duplicate {
			return fmt.Errorf("image catalog index has duplicate platform %q", key)
		}
		seen[key] = struct{}{}
		selection, exists := selections[key]
		if !exists {
			return fmt.Errorf("image catalog index is missing selection for platform %q", key)
		}
		if err := validate(selection); err != nil {
			return err
		}
		if !sameDescriptor(selection.Root, root) {
			return fmt.Errorf("image catalog selection for platform %q does not match its index root", key)
		}
		if !sameDescriptor(selection.Manifest, manifest) {
			return fmt.Errorf("image catalog selection for platform %q does not match its index descriptor", key)
		}
	}
	for key := range selections {
		platform, err := platforms.Parse(key)
		if err != nil {
			return fmt.Errorf("image catalog has invalid platform key %q: %w", key, err)
		}
		normalized, err := platformKey(platform)
		if err != nil || normalized != key {
			return fmt.Errorf("image catalog has non-normalized platform key %q", key)
		}
		if _, exists := seen[key]; !exists {
			return fmt.Errorf("image catalog index has unexpected selection for platform %q", key)
		}
	}
	return nil
}

func read(dir string) (catalog, error) {
	data, err := os.ReadFile(filepath.Join(dir, filename))
	if errors.Is(err, os.ErrNotExist) {
		return catalog{Version: version, Refs: map[string]string{}, RefPlatforms: map[string]string{}, Roots: map[string]rootRecord{}}, nil
	}
	if err != nil {
		return catalog{}, err
	}
	var value catalog
	if err := json.Unmarshal(data, &value); err != nil {
		return catalog{}, fmt.Errorf("decode image catalog: %w", err)
	}
	if value.Version != version || value.Refs == nil || value.Roots == nil {
		return catalog{}, fmt.Errorf("unsupported image catalog version %d", value.Version)
	}
	if value.RefPlatforms == nil {
		value.RefPlatforms = map[string]string{}
	}
	return value, nil
}

func write(dir string, value catalog) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(dir, ".catalog-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(file.Name()) }()
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return err
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(file.Name(), filepath.Join(dir, filename)); err != nil {
		return err
	}
	parent, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = parent.Close() }()
	return parent.Sync()
}

func withLock(ctx context.Context, dir string, exclusive bool, action func() error) error {
	if !filepath.IsAbs(dir) {
		return fmt.Errorf("image store directory must be absolute: %q", dir)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	lock := flock.New(filepath.Join(dir, ".catalog.lock"))
	var acquired bool
	var err error
	if exclusive {
		acquired, err = lock.TryLockContext(ctx, 25*time.Millisecond)
	} else {
		acquired, err = lock.TryRLockContext(ctx, 25*time.Millisecond)
	}
	if err != nil {
		return err
	}
	if !acquired {
		return errors.New("image catalog lock not acquired")
	}
	defer func() { _ = lock.Unlock() }()
	return action()
}

// List returns a validated snapshot of every mutable local image name.
func List(ctx context.Context, dir string) ([]Entry, error) {
	var entries []Entry
	err := withLock(ctx, dir, false, func() error {
		value, err := read(dir)
		if err != nil {
			return err
		}
		references := make([]string, 0, len(value.Refs))
		for reference := range value.Refs {
			references = append(references, reference)
		}
		sort.Strings(references)
		for _, reference := range references {
			record, ok := value.Roots[value.Refs[reference]]
			if !ok {
				return fmt.Errorf("image catalog reference %q has no root", reference)
			}
			platforms := make(map[string]Selection, len(record.Selections))
			for key, selection := range record.Selections {
				if err := validate(selection); err != nil {
					return err
				}
				selection.ConfigData = append(json.RawMessage(nil), selection.ConfigData...)
				platforms[key] = selection
			}
			entries = append(entries, Entry{Reference: reference, Root: record.Root, Platforms: platforms})
		}
		return nil
	})
	return entries, err
}

// RemoveReference removes only a mutable name. Immutable records remain until
// store pruning establishes that no name or live storage object needs them.
func RemoveReference(ctx context.Context, dir, reference string) (bool, error) {
	if reference == "" || strings.TrimSpace(reference) != reference || strings.HasPrefix(reference, "sha256:") {
		return false, fmt.Errorf("invalid mutable image reference %q", reference)
	}
	removed := false
	err := withLock(ctx, dir, true, func() error {
		value, err := read(dir)
		if err != nil {
			return err
		}
		if _, removed = value.Refs[reference]; !removed {
			return nil
		}
		delete(value.Refs, reference)
		delete(value.RefPlatforms, reference)
		return write(dir, value)
	})
	return removed, err
}

// NamedImageIDs returns the containers/storage image IDs protected by mutable
// catalog names. The snapshot is taken under the catalog read lock.
func NamedImageIDs(ctx context.Context, dir string) (map[string]bool, error) {
	ids := make(map[string]bool)
	err := withLock(ctx, dir, false, func() error {
		value, err := read(dir)
		if err != nil {
			return err
		}
		for _, rootKey := range value.Refs {
			record, ok := value.Roots[rootKey]
			if !ok {
				return fmt.Errorf("image catalog root %s is missing", rootKey)
			}
			for _, selection := range record.Selections {
				ids[selection.ImageID] = true
			}
		}
		return nil
	})
	return ids, err
}

// PruneMissing removes unnamed immutable catalog roots. retain identifies
// storage images which could not be removed because they are active or named
// outside this catalog; mutable catalog names protect their own roots directly.
func PruneMissing(ctx context.Context, dir string, retain map[string]bool, dryRun bool) (int, error) {
	removed := 0
	err := withLock(ctx, dir, true, func() error {
		value, err := read(dir)
		if err != nil {
			return err
		}
		namedRoots := make(map[string]bool, len(value.Refs))
		for _, rootKey := range value.Refs {
			namedRoots[rootKey] = true
		}
		for rootKey, record := range value.Roots {
			if namedRoots[rootKey] {
				continue
			}
			live := len(record.Selections) != 0
			for _, selection := range record.Selections {
				if !retain[selection.ImageID] {
					live = false
					break
				}
			}
			if live {
				continue
			}
			removed++
			if !dryRun {
				delete(value.Roots, rootKey)
			}
		}
		if dryRun || removed == 0 {
			return nil
		}
		return write(dir, value)
	})
	return removed, err
}

// Commit publishes an imported storage image after its bytes and OCI metadata
// are durable. A mutable reference may be empty for a digest-only build.
func Commit(ctx context.Context, dir, reference string, platform v1.Platform, selection Selection) error {
	return commit(ctx, dir, reference, platform, selection, false)
}

// CommitSelected publishes a mutable reference restricted to one platform.
// Immutable digest records still retain every cataloged platform selection.
func CommitSelected(ctx context.Context, dir, reference string, platform v1.Platform, selection Selection) error {
	if reference == "" {
		return errors.New("selected image reference is required")
	}
	return commit(ctx, dir, reference, platform, selection, true)
}

func commit(ctx context.Context, dir, reference string, platform v1.Platform, selection Selection, restrictReference bool) error {
	if err := validate(selection); err != nil {
		return err
	}
	key, err := platformKey(platform)
	if err != nil {
		return err
	}
	if reference != "" && strings.TrimSpace(reference) != reference {
		return fmt.Errorf("invalid image reference %q", reference)
	}
	return withLock(ctx, dir, true, func() error {
		value, err := read(dir)
		if err != nil {
			return err
		}
		rootKey := selection.Root.Digest.String()
		record := value.Roots[rootKey]
		if record.Selections == nil {
			record = rootRecord{Root: selection.Root, Selections: map[string]Selection{}}
		} else if record.Root.Digest != selection.Root.Digest || record.Root.Size != selection.Root.Size || record.Root.MediaType != selection.Root.MediaType {
			return fmt.Errorf("image root %s conflicts with catalog", rootKey)
		}
		record.Selections[key] = selection
		if len(record.IndexData) != 0 {
			if err := validateIndex(record.Root, record.IndexData, record.Selections); err != nil {
				return err
			}
		}
		value.Roots[rootKey] = record
		// A selected platform manifest is also directly addressable by digest.
		if selection.Manifest.Digest != selection.Root.Digest {
			selectedKey := selection.Manifest.Digest.String()
			selected := value.Roots[selectedKey]
			if selected.Selections == nil {
				selected = rootRecord{Root: selection.Manifest, Selections: map[string]Selection{}}
			} else if selected.Root.Digest != selection.Manifest.Digest || selected.Root.Size != selection.Manifest.Size || selected.Root.MediaType != selection.Manifest.MediaType {
				return fmt.Errorf("image manifest %s conflicts with catalog", selectedKey)
			}
			selected.Selections[key] = Selection{
				Root: selection.Manifest, Manifest: selection.Manifest, ImageID: selection.ImageID, ConfigData: selection.ConfigData,
			}
			value.Roots[selectedKey] = selected
		}
		if reference != "" {
			value.Refs[reference] = rootKey
			if restrictReference {
				value.RefPlatforms[reference] = key
			} else {
				delete(value.RefPlatforms, reference)
			}
		}
		return write(dir, value)
	})
}

// CommitIndex atomically publishes a complete locally assembled OCI or Docker
// index, its platform selections, immutable manifest aliases, and an optional
// mutable reference. indexData is retained byte-for-byte for later export.
func CommitIndex(ctx context.Context, dir, reference string, root v1.Descriptor, indexData []byte, selections map[string]Selection) error {
	if reference != "" && strings.TrimSpace(reference) != reference {
		return fmt.Errorf("invalid image reference %q", reference)
	}
	if err := validateIndex(root, indexData, selections); err != nil {
		return err
	}
	return withLock(ctx, dir, true, func() error {
		value, err := read(dir)
		if err != nil {
			return err
		}
		rootKey := root.Digest.String()
		if existing, exists := value.Roots[rootKey]; exists && !sameDescriptor(existing.Root, root) {
			return fmt.Errorf("image root %s conflicts with catalog", rootKey)
		}

		record := rootRecord{
			Root:       root,
			Selections: make(map[string]Selection, len(selections)),
			IndexData:  append([]byte(nil), indexData...),
		}
		for key, selection := range selections {
			record.Selections[key] = selection
			selectedKey := selection.Manifest.Digest.String()
			selected := value.Roots[selectedKey]
			if selected.Selections == nil {
				selected = rootRecord{Root: selection.Manifest, Selections: map[string]Selection{}}
			} else if !sameDescriptor(selected.Root, selection.Manifest) {
				return fmt.Errorf("image manifest %s conflicts with catalog", selectedKey)
			}
			selected.Selections[key] = Selection{
				Root: selection.Manifest, Manifest: selection.Manifest, ImageID: selection.ImageID, ConfigData: selection.ConfigData,
			}
			value.Roots[selectedKey] = selected
		}
		value.Roots[rootKey] = record
		if reference != "" {
			value.Refs[reference] = rootKey
			delete(value.RefPlatforms, reference)
		}
		return write(dir, value)
	})
}

// LookupIndex finds a complete locally assembled index by local tag or root
// digest. Externally imported indexes committed one platform at a time do not
// retain index bytes and therefore return found=false.
func LookupIndex(ctx context.Context, dir, selector string) (v1.Descriptor, []byte, map[string]Selection, bool, error) {
	var root v1.Descriptor
	var indexData []byte
	var selections map[string]Selection
	var found bool
	err := withLock(ctx, dir, false, func() error {
		value, err := read(dir)
		if err != nil {
			return err
		}
		rootKey := selector
		if !strings.HasPrefix(rootKey, "sha256:") {
			rootKey = value.Refs[selector]
			if value.RefPlatforms[selector] != "" {
				return nil
			}
		}
		record, exists := value.Roots[rootKey]
		if !exists || len(record.IndexData) == 0 {
			return nil
		}
		if err := validateIndex(record.Root, record.IndexData, record.Selections); err != nil {
			return err
		}
		root = record.Root
		indexData = append([]byte(nil), record.IndexData...)
		selections = make(map[string]Selection, len(record.Selections))
		for key, selection := range record.Selections {
			selection.ConfigData = append(json.RawMessage(nil), selection.ConfigData...)
			selections[key] = selection
		}
		found = true
		return nil
	})
	return root, indexData, selections, found, err
}

// AvailablePlatforms reports cached selections, including registry indexes
// imported one platform at a time. Complete index bytes are only needed when
// exporting the full index, not when selecting an already available base.
func AvailablePlatforms(ctx context.Context, dir, selector string) ([]v1.Platform, error) {
	var result []v1.Platform
	err := withLock(ctx, dir, false, func() error {
		value, err := read(dir)
		if err != nil {
			return err
		}
		rootKey, restricted := selector, ""
		if !strings.HasPrefix(selector, "sha256:") {
			rootKey, restricted = value.Refs[selector], value.RefPlatforms[selector]
		}
		record, exists := value.Roots[rootKey]
		if !exists {
			return nil
		}
		for key, selection := range record.Selections {
			if restricted != "" && restricted != key {
				continue
			}
			if err := validate(selection); err != nil {
				return err
			}
			if !sameDescriptor(selection.Root, record.Root) {
				return errors.New("image catalog selection does not match its root")
			}
			platform, err := platforms.Parse(key)
			if err != nil {
				return err
			}
			result = append(result, platform)
		}
		return nil
	})
	sort.Slice(result, func(i, j int) bool { return platforms.Format(result[i]) < platforms.Format(result[j]) })
	return result, err
}

// Lookup finds an immutable platform selection by local tag or OCI root digest.
func Lookup(ctx context.Context, dir, selector string, platform v1.Platform) (Selection, bool, error) {
	key, err := platformKey(platform)
	if err != nil {
		return Selection{}, false, err
	}
	var found Selection
	var ok bool
	err = withLock(ctx, dir, false, func() error {
		value, err := read(dir)
		if err != nil {
			return err
		}
		rootKey := selector
		referencePlatform := ""
		if !strings.HasPrefix(rootKey, "sha256:") {
			rootKey = value.Refs[selector]
			referencePlatform = value.RefPlatforms[selector]
			if referencePlatform != "" && referencePlatform != key {
				return nil
			}
		}
		record, exists := value.Roots[rootKey]
		if !exists {
			return nil
		}
		found, ok = record.Selections[key]
		if !ok {
			return nil
		}
		if err := validate(found); err != nil {
			return err
		}
		if found.Root.Digest != record.Root.Digest || found.Root.Size != record.Root.Size || found.Root.MediaType != record.Root.MediaType {
			return errors.New("image catalog selection does not match its root")
		}
		return nil
	})
	return found, ok, err
}

// LookupSole finds a local tag or root digest when it exposes exactly one
// platform. It allows a foreign-platform image to be copied by name without
// requiring the caller to repeat the platform used at build time.
func LookupSole(ctx context.Context, dir, selector string) (Selection, v1.Platform, bool, error) {
	var found Selection
	var foundPlatform v1.Platform
	var ok bool
	err := withLock(ctx, dir, false, func() error {
		value, err := read(dir)
		if err != nil {
			return err
		}
		rootKey := selector
		referencePlatform := ""
		if !strings.HasPrefix(rootKey, "sha256:") {
			rootKey = value.Refs[selector]
			referencePlatform = value.RefPlatforms[selector]
		}
		record, exists := value.Roots[rootKey]
		if !exists || (referencePlatform == "" && len(record.Selections) != 1) {
			return nil
		}
		keys := make([]string, 0, 1)
		if referencePlatform != "" {
			keys = append(keys, referencePlatform)
		} else {
			for key := range record.Selections {
				keys = append(keys, key)
			}
		}
		for _, key := range keys {
			selection, exists := record.Selections[key]
			if !exists {
				return fmt.Errorf("image catalog reference %q lacks platform %q", selector, key)
			}
			platform, err := platforms.Parse(key)
			if err != nil {
				return fmt.Errorf("image catalog has invalid platform key %q: %w", key, err)
			}
			if err := validate(selection); err != nil {
				return err
			}
			if selection.Root.Digest != record.Root.Digest || selection.Root.Size != record.Root.Size || selection.Root.MediaType != record.Root.MediaType {
				return errors.New("image catalog selection does not match its root")
			}
			found, foundPlatform, ok = selection, platforms.Normalize(platform), true
		}
		return nil
	})
	return found, foundPlatform, ok, err
}

// LookupManifest finds an exact single-platform manifest digest without
// requiring the caller to already know the platform key under which it was
// cataloged. Index digests deliberately remain platform-selected via Lookup.
func LookupManifest(ctx context.Context, dir, selector string) (Selection, v1.Platform, bool, error) {
	wanted := digest.Digest(selector)
	if wanted.Validate() != nil {
		return Selection{}, v1.Platform{}, false, nil
	}
	var found Selection
	var foundPlatform v1.Platform
	var ok bool
	err := withLock(ctx, dir, false, func() error {
		value, err := read(dir)
		if err != nil {
			return err
		}
		record, exists := value.Roots[wanted.String()]
		if !exists || record.Root.Digest != wanted || record.Root.MediaType != v1.MediaTypeImageManifest && record.Root.MediaType != dockerManifestMediaType {
			return nil
		}
		keys := make([]string, 0, len(record.Selections))
		for key := range record.Selections {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		if len(keys) == 0 {
			return nil
		}
		selection := record.Selections[keys[0]]
		selectedPlatform, err := platforms.Parse(keys[0])
		if err != nil {
			return fmt.Errorf("image catalog has invalid platform key %q: %w", keys[0], err)
		}
		if err := validate(selection); err != nil {
			return err
		}
		if selection.Root.Digest != record.Root.Digest || selection.Root.Size != record.Root.Size || selection.Root.MediaType != record.Root.MediaType ||
			selection.Manifest.Digest != record.Root.Digest || selection.Manifest.Size != record.Root.Size || selection.Manifest.MediaType != record.Root.MediaType {
			return errors.New("image catalog selection does not match its manifest root")
		}
		found, foundPlatform, ok = selection, platforms.Normalize(selectedPlatform), true
		return nil
	})
	return found, foundPlatform, ok, err
}
