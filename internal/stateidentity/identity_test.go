package stateidentity

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func rootHeader() *tar.Header {
	return &tar.Header{Name: "/", Typeflag: tar.TypeDir, Mode: 0755, Uid: 0, Gid: 0, ModTime: time.Unix(1730000000, 123456789)}
}

type tarItem struct {
	header  tar.Header
	content string
}

func archive(t *testing.T, items ...tarItem) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := tar.NewWriter(&buf)
	for _, item := range items {
		item.header.Size = int64(len(item.content))
		if item.header.Format == tar.FormatUnknown {
			item.header.Format = tar.FormatPAX
		}
		if err := w.WriteHeader(&item.header); err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(item.content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func basicItems() []tarItem {
	mtime := time.Unix(1730000001, 987654321)
	return []tarItem{
		{header: tar.Header{Name: "a/", Typeflag: tar.TypeDir, Mode: 0710, ModTime: mtime}},
		{header: tar.Header{Name: "b/", Typeflag: tar.TypeDir, Mode: 0755, ModTime: mtime}},
		{header: tar.Header{Name: "a/file", Typeflag: tar.TypeReg, Mode: 0640, Uid: 42, Gid: 43, ModTime: mtime, PAXRecords: map[string]string{"SCHILY.xattr.user.binary": "\x00\xff\x80"}}, content: "bytes"},
		{header: tar.Header{Name: "b/link", Typeflag: tar.TypeLink, Linkname: "a/file", Mode: 0640, Uid: 42, Gid: 43, ModTime: mtime, PAXRecords: map[string]string{"SCHILY.xattr.user.binary": "\x00\xff\x80"}}},
		{header: tar.Header{Name: "b/symlink", Typeflag: tar.TypeSymlink, Linkname: "../a/file", Mode: 0777, ModTime: mtime}},
	}
}

func calc(t *testing.T, items []tarItem, config string) Identity {
	t.Helper()
	id, err := Calculate(context.Background(), rootHeader(), bytes.NewReader(archive(t, items...)), json.RawMessage(config), nil)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestCanonicalArchiveOrderingAndRepresentation(t *testing.T) {
	items := basicItems()
	a := calc(t, items, `{"architecture":"amd64","rootfs":{"type":"layers","diff_ids":["sha256:a"]},"history":[{"created_by":"RUN a"}],"config":{"Labels":{"a":"b"}},"vendor":{"n":1.0}}`)
	permuted := []tarItem{items[4], items[3], items[2], items[1], items[0]}
	permuted[2].header.Name = "./a/file"
	b := calc(t, permuted, ` { "vendor": {"n":1e0}, "config":{"Labels":{"a":"b"}}, "history":[{"created_by":"RUN b"}], "rootfs":{"diff_ids":["sha256:b"], "type":"layers"}, "architecture":"amd64" } `)
	if a != b {
		t.Fatalf("logical state changed with tar ordering/format or JSON provenance: %v vs %v", a, b)
	}
	if err := a.State.Validate(); err != nil {
		t.Fatalf("invalid digest: %v", err)
	}
}

func TestHardlinkLeaderChoiceDoesNotChangeIdentity(t *testing.T) {
	items := basicItems()
	first := calc(t, items, `{}`)
	items[2].header.Typeflag = tar.TypeLink
	items[2].header.Linkname = "b/link"
	items[2].content = ""
	items[3].header.Typeflag = tar.TypeReg
	items[3].header.Linkname = ""
	items[3].content = "bytes"
	second := calc(t, items, `{}`)
	if first != second {
		t.Fatalf("equivalent hardlink group has different identity: %v vs %v", first, second)
	}
}

func TestLargeHardlinkGroupAndChain(t *testing.T) {
	const links = 4096
	direct := make([]tarItem, 0, links+1)
	chain := make([]tarItem, 0, links+1)
	file := tarItem{header: tar.Header{Name: "file", Typeflag: tar.TypeReg, Mode: 0644}, content: "shared"}
	direct = append(direct, file)
	chain = append(chain, file)
	for i := range links {
		name := fmt.Sprintf("link-%04d", i)
		direct = append(direct, tarItem{header: tar.Header{Name: name, Typeflag: tar.TypeLink, Linkname: "file"}})
		target := "file"
		if i > 0 {
			target = fmt.Sprintf("link-%04d", i-1)
		}
		chain = append(chain, tarItem{header: tar.Header{Name: name, Typeflag: tar.TypeLink, Linkname: target}})
	}
	first := calc(t, direct, `{}`)
	second := calc(t, chain, `{}`)
	if first != second {
		t.Fatalf("same large inode group with direct and chained links differs: %v vs %v", first, second)
	}
	chain[links].header.Typeflag = tar.TypeReg
	chain[links].header.Linkname = ""
	chain[links].content = "shared"
	changed := calc(t, chain, `{}`)
	if changed.Filesystem == first.Filesystem {
		t.Fatal("splitting one member into a separate inode did not change topology")
	}
}

type cancelAfterChecks struct {
	context.Context
	checks int
	limit  int
}

func (c *cancelAfterChecks) Err() error {
	c.checks++
	if c.checks >= c.limit {
		return context.Canceled
	}
	return nil
}

func TestHardlinkLeaderCancellation(t *testing.T) {
	entries := map[string]*entry{
		"file": {header: &tar.Header{Typeflag: tar.TypeReg}},
	}
	for i := range 4096 {
		name := fmt.Sprintf("link-%04d", i)
		target := "file"
		if i > 0 {
			target = fmt.Sprintf("link-%04d", i-1)
		}
		entries[name] = &entry{header: &tar.Header{Typeflag: tar.TypeLink}, link: target}
	}
	ctx := &cancelAfterChecks{Context: context.Background(), limit: 128}
	_, err := regularLeader(ctx, "link-4095", entries, make(map[string]string))
	if err != context.Canceled {
		t.Fatalf("want cancellation while resolving hardlinks, got %v", err)
	}
	if ctx.checks != ctx.limit {
		t.Fatalf("leader resolution performed %d context checks, want %d before cancellation", ctx.checks, ctx.limit)
	}
}

func TestTarHeaderFormatDoesNotChangeIdentity(t *testing.T) {
	item := tarItem{header: tar.Header{Name: "plain", Typeflag: tar.TypeReg, Mode: 0644, ModTime: time.Unix(1700000000, 0)}, content: "same"}
	first := calc(t, []tarItem{item}, `{}`)
	item.header.Format = tar.FormatUSTAR
	second := calc(t, []tarItem{item}, `{}`)
	if first != second {
		t.Fatalf("equivalent PAX and USTAR exports differ: %v vs %v", first, second)
	}
}

func TestIdentityChangesForEffectiveState(t *testing.T) {
	baseline := basicItems()
	base := calc(t, baseline, `{"architecture":"amd64","created":"2026-01-01T00:00:00Z","config":{"Env":["A=1"]}}`)
	cases := map[string]func([]tarItem){
		"content": func(x []tarItem) { x[2].content = "other" },
		"mode":    func(x []tarItem) { x[2].header.Mode = 0600; x[3].header.Mode = 0600 },
		"uid":     func(x []tarItem) { x[2].header.Uid = 44; x[3].header.Uid = 44 },
		"gid":     func(x []tarItem) { x[2].header.Gid = 44; x[3].header.Gid = 44 },
		"mtime nanos": func(x []tarItem) {
			x[2].header.ModTime = x[2].header.ModTime.Add(100 * time.Nanosecond)
			x[3].header.ModTime = x[3].header.ModTime.Add(100 * time.Nanosecond)
		},
		"directory mtime nanos": func(x []tarItem) {
			x[0].header.ModTime = x[0].header.ModTime.Add(time.Nanosecond)
		},
		"hardlink topology": func(x []tarItem) {
			x[3].header.Typeflag = tar.TypeReg
			x[3].header.Linkname = ""
			x[3].content = "bytes"
		},
		"symlink target": func(x []tarItem) { x[4].header.Linkname = "../a/other" },
		"xattr bytes": func(x []tarItem) {
			x[2].header.PAXRecords = map[string]string{"SCHILY.xattr.user.binary": "\x00\xff\x81"}
			x[3].header.PAXRecords = x[2].header.PAXRecords
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			items := basicItems()
			mutate(items)
			got := calc(t, items, `{"architecture":"amd64","created":"2026-01-01T00:00:00Z","config":{"Env":["A=1"]}}`)
			if got.Filesystem == base.Filesystem || got.State == base.State {
				t.Fatalf("%s did not invalidate filesystem state", name)
			}
			if got.Configuration != base.Configuration {
				t.Fatal("filesystem change altered config identity")
			}
		})
	}
	configChanged := calc(t, baseline, `{"architecture":"arm64","created":"2026-01-01T00:00:00Z","config":{"Env":["A=1"]}}`)
	if configChanged.Configuration == base.Configuration || configChanged.State == base.State {
		t.Fatal("platform config change did not invalidate state")
	}
	if configChanged.Filesystem != base.Filesystem {
		t.Fatal("config change altered filesystem identity")
	}
}

func TestRootAndAmbientSELinuxIdentity(t *testing.T) {
	items := basicItems()
	data := archive(t, items...)
	root := rootHeader()
	base, err := Calculate(context.Background(), root, bytes.NewReader(data), json.RawMessage(`{}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	changed := *root
	changed.Mode = 0700
	other, err := Calculate(context.Background(), &changed, bytes.NewReader(data), json.RawMessage(`{}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if other.Filesystem == base.Filesystem {
		t.Fatal("root mode did not invalidate filesystem identity")
	}
	changed = *root
	changed.Uid = 42
	other, err = Calculate(context.Background(), &changed, bytes.NewReader(data), json.RawMessage(`{}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if other.Filesystem == base.Filesystem {
		t.Fatal("root owner did not invalidate filesystem identity")
	}
	changed = *root
	changed.PAXRecords = map[string]string{"SCHILY.xattr.user.coopr": "value"}
	other, err = Calculate(context.Background(), &changed, bytes.NewReader(data), json.RawMessage(`{}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if other.Filesystem == base.Filesystem {
		t.Fatal("root xattr did not invalidate filesystem identity")
	}
	changed = *root
	changed.ModTime = changed.ModTime.Add(123 * time.Nanosecond)
	other, err = Calculate(context.Background(), &changed, bytes.NewReader(data), json.RawMessage(`{}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if other != base {
		t.Fatal("engine-owned root mtime changed portable identity")
	}
	rootEntry := changed
	rootEntry.Name = "."
	withRoot := append([]tarItem{{header: rootEntry}}, items...)
	other, err = Calculate(context.Background(), root, bytes.NewReader(archive(t, withRoot...)), json.RawMessage(`{}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if other != base {
		t.Fatal("root tar entry mtime changed portable identity")
	}
	withRoot[0].header.Mode = 0700
	if _, err := Calculate(context.Background(), root, bytes.NewReader(archive(t, withRoot...)), json.RawMessage(`{}`), nil); err == nil {
		t.Fatal("root tar entry with conflicting mode was accepted")
	}
	root.PAXRecords = map[string]string{"SCHILY.xattr.security.selinux": "ambient\x00"}
	ambient, err := Calculate(context.Background(), root, bytes.NewReader(data), json.RawMessage(`{}`), []byte("ambient\x00"))
	if err != nil {
		t.Fatal(err)
	}
	if ambient != base {
		t.Fatal("measured ambient SELinux label changed portable identity")
	}
	if _, err := Calculate(context.Background(), root, bytes.NewReader(data), json.RawMessage(`{}`), []byte("other\x00")); err == nil {
		t.Fatal("unexpected SELinux label accepted")
	}
}

func TestGatewayModTimesRestoreTarNanoseconds(t *testing.T) {
	second := time.Unix(1735689600, 0)
	items := []tarItem{
		{header: tar.Header{Name: "dir/", Typeflag: tar.TypeDir, Mode: 0755, ModTime: second}},
		{header: tar.Header{Name: "dir/file", Typeflag: tar.TypeReg, Mode: 0644, ModTime: second}, content: "same"},
	}
	data := archive(t, items...)
	calculate := func(t *testing.T, opts ...Option) Identity {
		t.Helper()
		id, err := Calculate(context.Background(), rootHeader(), bytes.NewReader(data), json.RawMessage(`{}`), nil, opts...)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	firstTimes := map[string]time.Time{"dir": second.Add(123456789 * time.Nanosecond), "dir/file": second.Add(123456789 * time.Nanosecond)}
	first := calculate(t, WithModTimes(firstTimes))
	fileChanged := map[string]time.Time{"dir": firstTimes["dir"], "dir/file": firstTimes["dir/file"].Add(time.Nanosecond)}
	file := calculate(t, WithModTimes(fileChanged))
	if file.Filesystem == first.Filesystem || file.State == first.State {
		t.Fatal("gateway file mtime nanosecond did not change identity")
	}
	dirChanged := map[string]time.Time{"dir": firstTimes["dir"].Add(time.Nanosecond), "dir/file": firstTimes["dir/file"]}
	dir := calculate(t, WithModTimes(dirChanged))
	if dir.Filesystem == first.Filesystem || dir.State == first.State {
		t.Fatal("gateway directory mtime nanosecond did not change identity")
	}
	raw := calculate(t)
	if raw == first {
		t.Fatal("rounded raw tar unexpectedly includes gateway nanoseconds")
	}
}

func TestGatewayModTimesRequireExactArchivePathsAndSeconds(t *testing.T) {
	second := time.Unix(1735689600, 0)
	data := archive(t, tarItem{header: tar.Header{Name: "file", Typeflag: tar.TypeReg, Mode: 0644, ModTime: second}, content: "x"})
	for _, tc := range []struct {
		name  string
		times map[string]time.Time
		want  string
	}{
		{"missing", map[string]time.Time{}, "missing gateway mtime"},
		{"extra", map[string]time.Time{"file": second, "extra": second}, "absent from rootfs tar"},
		{"root", map[string]time.Time{".": second, "file": second}, "non-canonical mtime path"},
		{"alias", map[string]time.Time{"./file": second, "file": second}, "non-canonical mtime path"},
		{"trailing slash", map[string]time.Time{"file/": second}, "non-canonical mtime path"},
		{"different seconds", map[string]time.Time{"file": second.Add(time.Second)}, "mtime seconds"},
		{"nil", nil, "mtime metadata is nil"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Calculate(context.Background(), rootHeader(), bytes.NewReader(data), json.RawMessage(`{}`), nil, WithModTimes(tc.times))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %q, got %v", tc.want, err)
			}
		})
	}
	_, err := Calculate(context.Background(), rootHeader(), bytes.NewReader(data), json.RawMessage(`{}`), nil, WithModTimes(map[string]time.Time{"file": second}), WithModTimes(map[string]time.Time{"file": second}))
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate option accepted: %v", err)
	}
}

func TestMalformedTarAndConfigRejected(t *testing.T) {
	items := basicItems()
	good := archive(t, items...)
	cases := map[string]struct {
		items  []tarItem
		raw    []byte
		config string
	}{
		"duplicate path":       {items: append(basicItems(), basicItems()[2]), config: `{}`},
		"duplicate root":       {items: []tarItem{{header: *rootHeader()}, {header: *rootHeader()}}, config: `{}`},
		"escaping path":        {items: []tarItem{{header: tar.Header{Name: "../escape", Typeflag: tar.TypeReg, Mode: 0644}, content: "x"}}, config: `{}`},
		"missing hardlink":     {items: []tarItem{{header: tar.Header{Name: "dangling", Typeflag: tar.TypeLink, Linkname: "absent"}}}, config: `{}`},
		"unsupported pax":      {items: []tarItem{{header: tar.Header{Name: "file", Typeflag: tar.TypeReg, PAXRecords: map[string]string{"vendor.opaque": "lost"}}}}, config: `{}`},
		"truncated file":       {raw: good[:1537], config: `{}`},
		"missing trailer":      {raw: good[:len(good)-1024], config: `{}`},
		"duplicate config key": {raw: good, config: `{"a":1,"a":2}`},
		"invalid config":       {raw: good, config: `{"a":`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			data := tc.raw
			if tc.items != nil {
				data = archive(t, tc.items...)
			}
			_, err := Calculate(context.Background(), rootHeader(), bytes.NewReader(data), json.RawMessage(tc.config), nil)
			if err == nil {
				t.Fatal("malformed input accepted")
			}
		})
	}
}

func TestDeviceAndUnknownConfigFieldsMatter(t *testing.T) {
	device := []tarItem{{header: tar.Header{Name: "dev", Typeflag: tar.TypeChar, Mode: 0600, Devmajor: 1, Devminor: 3}}}
	first := calc(t, device, `{"vendor":{"nested":{"value":"a"}}}`)
	device[0].header.Devminor = 5
	second := calc(t, device, `{"vendor":{"nested":{"value":"a"}}}`)
	if first.Filesystem == second.Filesystem {
		t.Fatal("device number did not change filesystem identity")
	}
	third := calc(t, device, `{"vendor":{"nested":{"value":"b"}}}`)
	if second.Configuration == third.Configuration || second.State == third.State {
		t.Fatal("unknown config field did not change state identity")
	}
}

func TestCanonicalJSONNumbersWithoutFloatConversion(t *testing.T) {
	a := calc(t, nil, `{"n":123456789012345678901234567890.000,"z":-0,"rootfs":{"diff_ids":["x"],"type":"layers"}}`)
	b := calc(t, nil, `{"rootfs":{"type":"layers","diff_ids":["y"]},"z":0e99,"n":123456789012345678901234567890}`)
	if a != b {
		t.Fatal("exact decimal or zero numeric normalization differs")
	}
	c := calc(t, nil, `{"n":123456789012345678901234567891,"z":0,"rootfs":{"type":"layers"}}`)
	if c.Configuration == a.Configuration {
		t.Fatal("large integer rounded")
	}
}

func TestInvalidUTF8PathBytesRemainDistinct(t *testing.T) {
	base := []tarItem{{header: tar.Header{Name: "f\xff", Typeflag: tar.TypeReg, Mode: 0644}, content: "x"}}
	one := calc(t, base, `{}`)
	base[0].header.Name = "f\xfe"
	two := calc(t, base, `{}`)
	if one.Filesystem == two.Filesystem {
		t.Fatal("distinct invalid UTF-8 path bytes collapsed")
	}
}

func TestCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Calculate(ctx, rootHeader(), strings.NewReader(""), json.RawMessage(`{}`), nil)
	if err != context.Canceled {
		t.Fatalf("want cancellation, got %v", err)
	}
}
