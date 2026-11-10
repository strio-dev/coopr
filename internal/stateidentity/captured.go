package stateidentity

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/opencontainers/go-digest"
)

// CapturedEntry binds final filesystem metadata to bytes hashed from the same owned capture.
// Headers must describe the same owned, unmodified capture as the payloads.
// This representation does not establish ownership of a mutable caller path.
type CapturedEntry struct {
	Header  *tar.Header
	Content digest.Digest
}

// CapturedIndex retains validated metadata and payload hashes for one owned,
// unmodified capture. It is not a memo for a mutable filesystem pathname.
type CapturedIndex struct {
	entries  map[string]*entry
	names    []string
	identity digest.Digest
}

// NewCapturedIndex binds complete materialized records, including root mtime.
// It shares binary-safe metadata and hardlink handling with Calculate while
// using a separate captured-context domain without reading payloads itself.
func NewCapturedIndex(ctx context.Context, records []CapturedEntry) (*CapturedIndex, error) {
	if ctx == nil {
		return nil, errors.New("captured filesystem identity requires context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	entries := make(map[string]*entry, len(records))
	for _, record := range records {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if record.Header == nil {
			return nil, errors.New("captured filesystem entry has no metadata")
		}
		header := *record.Header
		if header.Size < 0 {
			return nil, fmt.Errorf("captured filesystem entry %q has negative size", header.Name)
		}
		name, err := canonicalPath(header.Name, true)
		if err != nil {
			return nil, fmt.Errorf("captured filesystem path %q: %w", header.Name, err)
		}
		item, err := makeEntry(&header, name, nil)
		if err != nil {
			return nil, fmt.Errorf("captured filesystem entry %q: %w", name, err)
		}
		if _, duplicate := entries[name]; duplicate {
			return nil, fmt.Errorf("duplicate captured filesystem path %q", name)
		}
		if header.Typeflag == tar.TypeReg {
			if err := record.Content.Validate(); err != nil {
				return nil, fmt.Errorf("captured payload %q: %w", name, err)
			}
			if record.Content.Algorithm() != digest.SHA256 {
				return nil, fmt.Errorf("captured payload %q must use sha256", name)
			}
			item.content = record.Content
		} else if record.Content != "" {
			return nil, fmt.Errorf("non-file captured path %q has a payload digest", name)
		}
		entries[name] = item
	}
	identity, err := hashFilesystem(ctx, entries, "coopr.captured-context.fs.v1")
	if err != nil {
		return nil, err
	}
	return &CapturedIndex{entries: entries, names: slices.Sorted(maps.Keys(entries)), identity: identity}, nil
}

// Digest identifies the complete capture, retaining the original context domain.
func (index *CapturedIndex) Digest() digest.Digest { return index.identity }

// Select jointly identifies native-resolved bind sources without reading the
// filesystem. Cross-mount aliases and regular-file link counts remain observable
// even when some aliases are outside the selected subtrees.
func (index *CapturedIndex) Select(ctx context.Context, sources []string) (digest.Digest, error) {
	if ctx == nil || index == nil || len(sources) == 0 {
		return "", errors.New("context selection requires an owned index and sources")
	}
	leaders, counts := make(map[string]string), make(map[string]int64)
	for name, item := range index.entries {
		if item.header.Typeflag == tar.TypeReg || item.header.Typeflag == tar.TypeLink {
			leader, err := regularLeader(ctx, name, index.entries, leaders)
			if err != nil {
				return "", err
			}
			counts[leader]++
		}
	}
	projected := map[string]*entry{".": {header: &tar.Header{Name: ".", Typeflag: tar.TypeDir}, name: "."}}
	type group struct {
		paths []string
		links int64
	}
	groups := make(map[string]*group)
	for number, source := range sources {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		name, err := canonicalPath(source, true)
		if err != nil {
			return "", err
		}
		selected, found := index.entries[name]
		if !found {
			return "", fmt.Errorf("context bind source %q is unavailable in capture", source)
		}
		add := func(original string) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			item := index.entries[original]
			copy := *item
			header := *item.header
			copy.header = &header
			copy.name = strconv.Itoa(number)
			if original != name {
				relative := original
				if name != "." {
					relative = strings.TrimPrefix(original, name+"/")
				}
				copy.name += "/" + relative
			}
			key, links := "p:"+original, int64(0)
			if leader, regular := leaders[original]; regular {
				root := index.entries[leader]
				copy.content, copy.xattrs = root.content, root.xattrs
				*copy.header = *root.header
				copy.header.Typeflag, copy.header.Linkname, copy.link = tar.TypeReg, "", ""
				key, links = "r:"+leader, counts[leader]
			}
			copy.header.Name = copy.name
			projected[copy.name] = &copy
			g := groups[key]
			if g == nil {
				g = &group{links: links}
				groups[key] = g
			}
			g.paths = append(g.paths, copy.name)
			return nil
		}
		if err := add(name); err != nil {
			return "", err
		}
		if selected.header.Typeflag == tar.TypeDir {
			prefix, start := name+"/", 0
			if name != "." {
				start = sort.SearchStrings(index.names, prefix)
			}
			for _, child := range index.names[start:] {
				if name == "." {
					if child == "." {
						continue
					}
				} else if !strings.HasPrefix(child, prefix) {
					break
				}
				if err := add(child); err != nil {
					return "", err
				}
			}
		}
	}
	filesystem, err := hashFilesystem(ctx, projected, "coopr.captured-context.selection.fs.v1")
	if err != nil {
		return "", err
	}
	ordered := make([]*group, 0, len(groups))
	for _, g := range groups {
		slices.Sort(g.paths)
		ordered = append(ordered, g)
	}
	slices.SortFunc(ordered, func(a, b *group) int { return strings.Compare(a.paths[0], b.paths[0]) })
	hash := domainHash("coopr.captured-context.selection.v1")
	field(hash, []byte(filesystem))
	integer(hash, int64(len(ordered)))
	for _, g := range ordered {
		integer(hash, g.links)
		integer(hash, int64(len(g.paths)))
		for _, name := range g.paths {
			field(hash, []byte(name))
		}
	}
	return sum(hash), nil
}
