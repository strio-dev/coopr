// Package stateidentity hashes the logical contents of an effective rootfs and
// its image configuration. Its tar input is a complete filesystem export from
// the builder, never a layer diff. It parses the archive without extracting it.
//
// Version 2 includes file type, permissions, owner, nanosecond mtime for every
// non-root path, links, device numbers, xattrs, and regular-file bytes. The
// root directory's mtime is omitted because it belongs to the builder's
// snapshot mount and is absent from the exported OCI filesystem. Root mode,
// owner, and xattrs remain significant. Archive order, tar encoding,
// user/group names, and padding are not filesystem state. Access/change times
// and archive metadata that cannot be represented by this contract are rejected.
// A security.selinux xattr is excluded only if it byte-for-byte matches the
// separately measured ambient snapshot label. rootfs.diff_ids and history are
// layer provenance, so they are excluded from the logical configuration hash;
// callers must still retain the complete image config for publication.
//
// JSON objects are sorted recursively. JSON numbers are normalized as an exact
// signed decimal coefficient and arbitrary-precision base-10 exponent: 1,
// 1.0, and 1e0 share an identity without a float64 conversion.
package stateidentity

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"math/big"
	"path"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/opencontainers/go-digest"
)

const (
	filesystemDomain    = "coopr.stateidentity.fs.v2"
	configurationDomain = "coopr.stateidentity.config.v1"
	stateDomain         = "coopr.stateidentity.state.v2"
)

// Identity separates the effective filesystem, logical image configuration,
// and their combined state. Digests use SHA-256 with distinct versioned domains.
type Identity struct {
	Filesystem    digest.Digest
	Configuration digest.Digest
	State         digest.Digest
}

type entry struct {
	header  *tar.Header
	name    string
	link    string
	content digest.Digest
	xattrs  map[string]string
}

// Option supplies separately observed metadata for an identity calculation.
type Option func(*calculateOptions) error

type calculateOptions struct {
	modTimes    map[string]time.Time
	hasModTimes bool
}

// WithModTimes supplies nanosecond mtimes measured from the same immutable
// builder snapshot as the tar export. The tar exporter may round these values
// to whole seconds. Keys must be canonical relative paths for every non-root
// tar entry; the root directory's mtime is always excluded.
func WithModTimes(times map[string]time.Time) Option {
	return func(options *calculateOptions) error {
		if options.hasModTimes {
			return errors.New("duplicate mtime metadata option")
		}
		if times == nil {
			return errors.New("mtime metadata is nil")
		}
		options.hasModTimes = true
		options.modTimes = make(map[string]time.Time, len(times))
		for name, mtime := range times {
			canonical, err := canonicalPath(name, false)
			if err != nil || canonical != name {
				return fmt.Errorf("non-canonical mtime path %q", name)
			}
			options.modTimes[name] = mtime
		}
		return nil
	}
}

// Calculate computes a portable identity from a complete effective-rootfs tar.
// root is the builder's separately observed root-directory metadata. The
// optional root entry in archive must agree with it, except for root mtime.
// ambientSELinux must be measured independently from the builder's empty
// snapshot on this host. Use WithModTimes when the exporter loses nanoseconds.
func Calculate(ctx context.Context, root *tar.Header, archive io.Reader, config json.RawMessage, ambientSELinux []byte, opts ...Option) (Identity, error) {
	if ctx == nil || root == nil || archive == nil {
		return Identity{}, errors.New("state identity requires context, root header, and archive")
	}
	var options calculateOptions
	for _, option := range opts {
		if option == nil {
			return Identity{}, errors.New("nil state identity option")
		}
		if err := option(&options); err != nil {
			return Identity{}, err
		}
	}
	if err := ctx.Err(); err != nil {
		return Identity{}, err
	}
	rootCopy := *root
	rootCopy.Name = "."
	rootCopy.ModTime = time.Time{}
	if rootCopy.Typeflag != tar.TypeDir {
		return Identity{}, errors.New("root metadata is not a directory")
	}
	rootEntry, err := makeEntry(&rootCopy, ".", ambientSELinux)
	if err != nil {
		return Identity{}, fmt.Errorf("root metadata: %w", err)
	}
	entries := map[string]*entry{".": rootEntry}
	seenModTimes := make(map[string]struct{}, len(options.modTimes))
	rootSeen := false
	stream := &checkedReader{ctx: ctx, reader: archive}
	reader := tar.NewReader(stream)
	for {
		markerStart := stream.count
		stream.checkZeros = true
		stream.nonzero = false
		hdr, err := reader.Next()
		if errors.Is(err, io.EOF) {
			if _, err := io.Copy(io.Discard, stream); err != nil {
				return Identity{}, fmt.Errorf("read rootfs tar trailer: %w", err)
			}
			if stream.nonzero || stream.count-markerStart < 1024 || stream.count%512 != 0 {
				return Identity{}, errors.New("rootfs tar has a missing or malformed end marker")
			}
			break
		}
		stream.checkZeros = false
		if err != nil {
			return Identity{}, fmt.Errorf("read rootfs tar: %w", err)
		}
		name, err := canonicalPath(hdr.Name, true)
		if err != nil {
			return Identity{}, fmt.Errorf("rootfs tar path %q: %w", hdr.Name, err)
		}
		if name != "." && options.hasModTimes {
			mtime, ok := options.modTimes[name]
			if !ok {
				return Identity{}, fmt.Errorf("rootfs tar path %q is missing gateway mtime", name)
			}
			if hdr.ModTime.Unix() != mtime.Round(time.Second).Unix() {
				return Identity{}, fmt.Errorf("rootfs tar path %q has mtime seconds %d, gateway rounds to %d", name, hdr.ModTime.Unix(), mtime.Round(time.Second).Unix())
			}
			hdr.ModTime = mtime
			seenModTimes[name] = struct{}{}
		}
		item, err := makeEntry(hdr, name, ambientSELinux)
		if err != nil {
			return Identity{}, fmt.Errorf("rootfs tar entry %q: %w", hdr.Name, err)
		}
		if hdr.Typeflag == tar.TypeReg {
			h := sha256.New()
			if _, err := io.Copy(h, reader); err != nil {
				return Identity{}, fmt.Errorf("read file %q: %w", name, err)
			}
			item.content = sum(h)
		}
		if name == "." {
			if rootSeen {
				return Identity{}, errors.New("duplicate rootfs root entry")
			}
			rootSeen = true
			item.header.ModTime = time.Time{}
			if !sameMetadata(rootEntry, item) {
				return Identity{}, errors.New("archive root metadata differs from supplied root metadata")
			}
			continue
		}
		if _, exists := entries[name]; exists {
			return Identity{}, fmt.Errorf("duplicate rootfs path %q", name)
		}
		entries[name] = item
	}
	if options.hasModTimes && len(seenModTimes) != len(options.modTimes) {
		for name := range options.modTimes {
			if _, ok := seenModTimes[name]; !ok {
				return Identity{}, fmt.Errorf("gateway mtime path %q is absent from rootfs tar", name)
			}
		}
	}
	for name := range entries {
		if err := ctx.Err(); err != nil {
			return Identity{}, err
		}
		if name == "." {
			continue
		}
		parent := path.Dir(name)
		if parent == "." {
			continue
		}
		p, ok := entries[parent]
		if !ok || p.header.Typeflag != tar.TypeDir {
			return Identity{}, fmt.Errorf("%q has missing or non-directory parent %q", name, parent)
		}
	}
	leaders := make(map[string]string)
	groupIDs := make(map[string]string)
	for name, item := range entries {
		if err := ctx.Err(); err != nil {
			return Identity{}, err
		}
		if item.header.Typeflag != tar.TypeReg && item.header.Typeflag != tar.TypeLink {
			continue
		}
		leader, err := regularLeader(ctx, name, entries, leaders)
		if err != nil {
			return Identity{}, err
		}
		if item.header.Typeflag == tar.TypeLink && !compatibleLinkMetadata(item, entries[leader]) {
			return Identity{}, fmt.Errorf("hardlink %q has conflicting inode metadata", name)
		}
		if smallest, ok := groupIDs[leader]; !ok || name < smallest {
			groupIDs[leader] = name
		}
	}
	fsHash := domainHash(filesystemDomain)
	names := make([]string, 0, len(entries))
	for name := range entries {
		if err := ctx.Err(); err != nil {
			return Identity{}, err
		}
		names = append(names, name)
	}
	slices.Sort(names)
	field(fsHash, []byte("entries"))
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return Identity{}, err
		}
		item := entries[name]
		field(fsHash, []byte(name))
		if item.header.Typeflag == tar.TypeReg || item.header.Typeflag == tar.TypeLink {
			leader := leaders[name]
			writeMetadata(fsHash, entries[leader], tar.TypeReg)
			field(fsHash, []byte(entries[leader].content))
			field(fsHash, []byte(groupIDs[leader]))
		} else {
			writeMetadata(fsHash, item, item.header.Typeflag)
		}
	}
	configHash, err := hashConfig(config)
	if err != nil {
		return Identity{}, err
	}
	fsDigest := sum(fsHash)
	stateHash := domainHash(stateDomain)
	field(stateHash, []byte(fsDigest))
	field(stateHash, []byte(configHash))
	return Identity{Filesystem: fsDigest, Configuration: configHash, State: sum(stateHash)}, nil
}

type checkedReader struct {
	ctx        context.Context
	reader     io.Reader
	count      int64
	checkZeros bool
	nonzero    bool
}

func (r *checkedReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := r.reader.Read(p)
	r.count += int64(n)
	if r.checkZeros {
		for _, b := range p[:n] {
			if b != 0 {
				r.nonzero = true
				break
			}
		}
	}
	if err == nil && r.ctx.Err() != nil {
		return n, r.ctx.Err()
	}
	return n, err
}

func canonicalPath(name string, allowRoot bool) (string, error) {
	if name == "" || strings.ContainsRune(name, 0) {
		return "", errors.New("empty or NUL-containing path")
	}
	if name == "/" || name == "." || name == "./" {
		if allowRoot {
			return ".", nil
		}
		return "", errors.New("root path is not a hardlink target")
	}
	if strings.HasPrefix(name, "/") {
		return "", errors.New("absolute path")
	}
	for strings.HasPrefix(name, "./") {
		name = strings.TrimPrefix(name, "./")
	}
	parts := strings.Split(strings.TrimSuffix(name, "/"), "/")
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return "", errors.New("non-canonical or escaping path")
		}
	}
	return strings.Join(parts, "/"), nil
}

func makeEntry(hdr *tar.Header, name string, ambient []byte) (*entry, error) {
	if hdr == nil {
		return nil, errors.New("missing header")
	}
	// Legacy V7 archives encode a regular file with a zero type byte.
	if hdr.Typeflag == 0 {
		hdr.Typeflag = tar.TypeReg
	}
	if strings.HasSuffix(hdr.Name, "/") && name != "." && hdr.Typeflag != tar.TypeDir {
		return nil, errors.New("non-directory path has trailing slash")
	}
	if hdr.Uid < 0 || hdr.Gid < 0 || hdr.Devmajor < 0 || hdr.Devminor < 0 {
		return nil, errors.New("negative owner or device number")
	}
	switch hdr.Typeflag {
	case tar.TypeReg, tar.TypeDir, tar.TypeSymlink, tar.TypeLink, tar.TypeChar, tar.TypeBlock, tar.TypeFifo:
	default:
		return nil, fmt.Errorf("unsupported tar type %d", hdr.Typeflag)
	}
	if hdr.Typeflag != tar.TypeReg && hdr.Size != 0 {
		return nil, errors.New("non-file entry has size")
	}
	if hdr.Typeflag != tar.TypeChar && hdr.Typeflag != tar.TypeBlock && (hdr.Devmajor != 0 || hdr.Devminor != 0) {
		return nil, errors.New("device numbers on non-device entry")
	}
	if hdr.Typeflag != tar.TypeSymlink && hdr.Typeflag != tar.TypeLink && hdr.Linkname != "" {
		return nil, errors.New("link target on non-link entry")
	}
	if hdr.Typeflag == tar.TypeSymlink && (hdr.Linkname == "" || strings.ContainsRune(hdr.Linkname, 0)) {
		return nil, errors.New("empty or NUL-containing symlink target")
	}
	if !hdr.AccessTime.IsZero() || !hdr.ChangeTime.IsZero() {
		return nil, errors.New("access/change time metadata is unsupported")
	}
	attrs := make(map[string]string)
	for key, value := range hdr.PAXRecords {
		if strings.HasPrefix(key, "SCHILY.xattr.") {
			attr := strings.TrimPrefix(key, "SCHILY.xattr.")
			if attr == "" || strings.ContainsRune(attr, 0) {
				return nil, errors.New("invalid xattr name")
			}
			if attr == "security.selinux" {
				if len(ambient) == 0 || !bytes.Equal([]byte(value), ambient) {
					return nil, errors.New("security.selinux differs from measured ambient label")
				}
			} else {
				attrs[attr] = value
			}
			continue
		}
		switch key {
		case "path", "linkpath", "uid", "gid", "uname", "gname", "size", "mtime":
		default:
			return nil, fmt.Errorf("unsupported PAX metadata %q", key)
		}
	}
	item := &entry{header: hdr, name: name, xattrs: attrs}
	switch hdr.Typeflag {
	case tar.TypeLink:
		link, err := canonicalPath(hdr.Linkname, false)
		if err != nil {
			return nil, fmt.Errorf("hardlink target: %w", err)
		}
		item.link = link
	case tar.TypeSymlink:
		item.link = hdr.Linkname
	}
	return item, nil
}

func regularLeader(ctx context.Context, name string, entries map[string]*entry, memo map[string]string) (string, error) {
	chain := make([]string, 0)
	seen := make(map[string]bool)
	current := name
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if leader, ok := memo[current]; ok {
			for _, member := range chain {
				memo[member] = leader
			}
			return leader, nil
		}
		if seen[current] {
			return "", fmt.Errorf("hardlink cycle at %q", current)
		}
		seen[current] = true
		item, ok := entries[current]
		if !ok {
			return "", fmt.Errorf("hardlink target %q is missing", current)
		}
		chain = append(chain, current)
		switch item.header.Typeflag {
		case tar.TypeReg:
			for _, member := range chain {
				memo[member] = current
			}
			return current, nil
		case tar.TypeLink:
			current = item.link
		default:
			return "", fmt.Errorf("hardlink target %q is not a regular file", current)
		}
	}
}

func compatibleLinkMetadata(link, leader *entry) bool {
	h, src := link.header, leader.header
	if h.Mode != 0 && h.Mode != src.Mode {
		return false
	}
	if h.Uid != 0 && h.Uid != src.Uid {
		return false
	}
	if h.Gid != 0 && h.Gid != src.Gid {
		return false
	}
	if !h.ModTime.IsZero() && !h.ModTime.Equal(src.ModTime) {
		return false
	}
	if len(link.xattrs) != 0 && !equalAttrs(link.xattrs, leader.xattrs) {
		return false
	}
	return true
}

func sameMetadata(a, b *entry) bool {
	x, y := a.header, b.header
	return x.Typeflag == y.Typeflag && x.Mode == y.Mode && x.Uid == y.Uid && x.Gid == y.Gid && x.ModTime.Equal(y.ModTime) && equalAttrs(a.xattrs, b.xattrs)
}

func equalAttrs(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func writeMetadata(h hash.Hash, item *entry, kind byte) {
	hdr := item.header
	field(h, []byte{kind})
	integer(h, hdr.Mode)
	integer(h, int64(hdr.Uid))
	integer(h, int64(hdr.Gid))
	writeTime(h, hdr.ModTime)
	if kind == tar.TypeSymlink {
		field(h, []byte(item.link))
	}
	if kind == tar.TypeChar || kind == tar.TypeBlock {
		integer(h, hdr.Devmajor)
		integer(h, hdr.Devminor)
	}
	keys := make([]string, 0, len(item.xattrs))
	for key := range item.xattrs {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		field(h, []byte(key))
		field(h, []byte(item.xattrs[key]))
	}
	field(h, nil)
}

func writeTime(h hash.Hash, t time.Time) { integer(h, t.Unix()); integer(h, int64(t.Nanosecond())) }
func integer(h hash.Hash, n int64) {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], uint64(n))
	_, _ = h.Write(b[:])
}
func field(h hash.Hash, b []byte) {
	var length [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(length[:], uint64(len(b)))
	_, _ = h.Write(length[:n])
	_, _ = h.Write(b)
}
func domainHash(domain string) hash.Hash { h := sha256.New(); field(h, []byte(domain)); return h }
func sum(h hash.Hash) digest.Digest      { return digest.Digest(fmt.Sprintf("sha256:%x", h.Sum(nil))) }

func hashConfig(raw json.RawMessage) (digest.Digest, error) {
	if !utf8.Valid(raw) {
		return "", errors.New("image config contains invalid UTF-8")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	value, err := parseJSON(decoder)
	if err != nil {
		return "", fmt.Errorf("image config: %w", err)
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return "", errors.New("image config has trailing JSON data")
	}
	object, ok := value.(map[string]any)
	if !ok {
		return "", errors.New("image config must be a JSON object")
	}
	delete(object, "history")
	if rootfs, ok := object["rootfs"].(map[string]any); ok {
		delete(rootfs, "diff_ids")
	}
	h := domainHash(configurationDomain)
	if err := writeJSON(h, object); err != nil {
		return "", err
	}
	return sum(h), nil
}

func parseJSON(d *json.Decoder) (any, error) {
	tok, err := d.Token()
	if err != nil {
		return nil, err
	}
	switch tok {
	case json.Delim('{'):
		object := map[string]any{}
		for d.More() {
			keyToken, err := d.Token()
			if err != nil {
				return nil, err
			}
			key, ok := keyToken.(string)
			if !ok {
				return nil, errors.New("object key is not a string")
			}
			if _, exists := object[key]; exists {
				return nil, fmt.Errorf("duplicate JSON key %q", key)
			}
			value, err := parseJSON(d)
			if err != nil {
				return nil, err
			}
			object[key] = value
		}
		_, err := d.Token()
		return object, err
	case json.Delim('['):
		var list []any
		for d.More() {
			value, err := parseJSON(d)
			if err != nil {
				return nil, err
			}
			list = append(list, value)
		}
		_, err := d.Token()
		return list, err
	default:
		return tok, nil
	}
}

func writeJSON(h hash.Hash, value any) error {
	switch v := value.(type) {
	case nil:
		field(h, []byte("null"))
	case bool:
		if v {
			field(h, []byte("true"))
		} else {
			field(h, []byte("false"))
		}
	case string:
		field(h, []byte("string"))
		field(h, []byte(v))
	case json.Number:
		coefficient, exponent, err := canonicalNumber(string(v))
		if err != nil {
			return err
		}
		field(h, []byte("number"))
		field(h, []byte(coefficient))
		field(h, []byte(exponent))
	case []any:
		field(h, []byte("array"))
		integer(h, int64(len(v)))
		for _, child := range v {
			if err := writeJSON(h, child); err != nil {
				return err
			}
		}
	case map[string]any:
		field(h, []byte("object"))
		integer(h, int64(len(v)))
		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		for _, key := range keys {
			field(h, []byte(key))
			if err := writeJSON(h, v[key]); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("unsupported JSON value %T", v)
	}
	return nil
}

func canonicalNumber(value string) (string, string, error) {
	negative := strings.HasPrefix(value, "-")
	if negative {
		value = value[1:]
	}
	parts := strings.SplitN(strings.ToLower(value), "e", 2)
	exponent := new(big.Int)
	if len(parts) == 2 {
		if _, ok := exponent.SetString(parts[1], 10); !ok {
			return "", "", errors.New("invalid JSON exponent")
		}
	}
	decimal := strings.SplitN(parts[0], ".", 2)
	digits := decimal[0]
	if len(decimal) == 2 {
		digits += decimal[1]
		exponent.Sub(exponent, big.NewInt(int64(len(decimal[1]))))
	}
	digits = strings.TrimLeft(digits, "0")
	if digits == "" {
		return "0", "0", nil
	}
	trimmed := strings.TrimRight(digits, "0")
	exponent.Add(exponent, big.NewInt(int64(len(digits)-len(trimmed))))
	if negative {
		trimmed = "-" + trimmed
	}
	return trimmed, exponent.String(), nil
}
