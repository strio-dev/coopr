package stateidentity

import (
	"archive/tar"
	"context"
	"fmt"
	"maps"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
)

func capturedFilesystemIdentity(ctx context.Context, records []CapturedEntry) (digest.Digest, error) {
	index, err := NewCapturedIndex(ctx, records)
	if err != nil {
		return "", err
	}
	return index.Digest(), nil
}

func TestCapturedIndexSelectMetadataAndOutsideAliases(t *testing.T) {
	build := func(records []CapturedEntry, sources ...string) digest.Digest {
		t.Helper()
		index, err := NewCapturedIndex(t.Context(), records)
		if err != nil {
			t.Fatal(err)
		}
		result, err := index.Select(t.Context(), sources)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	baseline := build(capturedRecords(), "a/file")
	if alias := build(capturedRecords(), "b/link"); alias != baseline {
		t.Fatal("equivalent selected hardlink views differ", baseline, alias)
	}
	records := capturedRecords()
	records[0].Header.ModTime = records[0].Header.ModTime.Add(time.Second)
	records[2].Header.ModTime = records[2].Header.ModTime.Add(time.Second)
	records = append(records, CapturedEntry{Header: &tar.Header{Name: "b/unbound", Typeflag: tar.TypeReg, Size: 5}, Content: digest.FromString("other")})
	if changed := build(records, "a/file"); changed != baseline {
		t.Fatal("unbound metadata/payload invalidated selected file", baseline, changed)
	}
	if before, after := build(capturedRecords(), "."), build(records, "."); before == after {
		t.Fatal("full-root view omitted observable changes")
	}
	outside := *records[4].Header
	outside.Name = "b/extra-link"
	records = append(records, CapturedEntry{Header: &outside})
	if changed := build(records, "a/file"); changed == baseline {
		t.Fatal("unbound alias did not invalidate observable link count")
	}
	records = capturedRecords()
	records[3].Header.PAXRecords = maps.Clone(records[3].Header.PAXRecords)
	records[3].Header.PAXRecords["SCHILY.xattr.user.binary"] = "\x00\xfe\x80"
	records[4].Header.PAXRecords = maps.Clone(records[3].Header.PAXRecords)
	if changed := build(records, "a/file"); changed == baseline {
		t.Fatal("selected binary xattr was omitted")
	}
	if direct, alias := build(capturedRecords(), "a", "a/file"), build(capturedRecords(), "a", "b/link"); direct != alias {
		t.Fatal("overlapping aliased views disagree", direct, alias)
	}
}

func TestCapturedIndexSelectCrossMountInodes(t *testing.T) {
	stamp := time.Unix(123456789, 0)
	file := func(name, link string) CapturedEntry {
		header := &tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0640, ModTime: stamp, Size: 7}
		content := digest.FromString("payload")
		if link != "" {
			header.Typeflag, header.Linkname, header.Size, content = tar.TypeLink, link, 0, ""
		}
		return CapturedEntry{Header: header, Content: content}
	}
	linked, err := NewCapturedIndex(t.Context(), []CapturedEntry{{Header: rootHeader()}, file("a", ""), file("b", "a")})
	if err != nil {
		t.Fatal(err)
	}
	independent, err := NewCapturedIndex(t.Context(), []CapturedEntry{{Header: rootHeader()}, file("a", ""), file("a-alias", "a"), file("b", ""), file("b-alias", "b")})
	if err != nil {
		t.Fatal(err)
	}
	selectFrom := func(index *CapturedIndex, sources ...string) digest.Digest {
		t.Helper()
		result, err := index.Select(t.Context(), sources)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	if selectFrom(linked, "a") != selectFrom(independent, "a") {
		t.Fatal("same single-file view and link count differ")
	}
	if selectFrom(linked, "a", "b") == selectFrom(independent, "a", "b") {
		t.Fatal("cross-mount equality lost despite equal individual link counts")
	}
	directories, err := NewCapturedIndex(t.Context(), []CapturedEntry{{Header: rootHeader()},
		{Header: &tar.Header{Name: "d", Typeflag: tar.TypeDir, Mode: 0755}},
		{Header: &tar.Header{Name: "e", Typeflag: tar.TypeDir, Mode: 0755}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if selectFrom(directories, "d", "d") == selectFrom(directories, "d", "e") {
		t.Fatal("cross-mount empty-directory equality was omitted")
	}
}

func TestCapturedIndexSelectRejectsMissingSources(t *testing.T) {
	index, err := NewCapturedIndex(t.Context(), capturedRecords())
	if err != nil {
		t.Fatal(err)
	}
	for _, paths := range [][]string{nil, {"missing"}, {"../outside"}, {"/absolute"}} {
		if _, err := index.Select(t.Context(), paths); err == nil {
			t.Fatalf("invalid selection accepted: %v", paths)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := index.Select(ctx, []string{"a/file"}); err == nil {
		t.Fatal("canceled selection succeeded")
	}
}

func TestCapturedIndexSelectionIsImmutableAndConcurrent(t *testing.T) {
	records := capturedRecords()
	index, err := NewCapturedIndex(t.Context(), records)
	if err != nil {
		t.Fatal(err)
	}
	want, err := index.Select(t.Context(), []string{"a", "b/link"})
	if err != nil {
		t.Fatal(err)
	}
	records[3].Header.Mode = 0
	records[3].Header.PAXRecords["SCHILY.xattr.user.binary"] = "changed caller metadata"
	var workers sync.WaitGroup
	failures := make(chan error, 16)
	for range cap(failures) {
		workers.Go(func() {
			got, err := index.Select(t.Context(), []string{"a", "b/link"})
			if err != nil || got != want {
				failures <- fmt.Errorf("immutable selection changed: got=%s want=%s error=%v", got, want, err)
			}
		})
	}
	workers.Wait()
	close(failures)
	for err := range failures {
		t.Error(err)
	}
}

func capturedRecords() []CapturedEntry {
	records := []CapturedEntry{{Header: rootHeader()}}
	for _, item := range basicItems() {
		header := item.header
		header.Size = int64(len(item.content))
		record := CapturedEntry{Header: &header}
		if header.Typeflag == tar.TypeReg {
			record.Content = digest.FromString(item.content)
		}
		records = append(records, record)
	}
	return records
}

func TestCapturedFilesystemPreservesMetadataAndDomains(t *testing.T) {
	records := capturedRecords()
	baseline, err := capturedFilesystemIdentity(t.Context(), records)
	if err != nil {
		t.Fatal(err)
	}
	if baseline == calc(t, basicItems(), `{}`).Filesystem {
		t.Fatal("captured input reused the image filesystem identity domain")
	}
	slices.Reverse(records)
	if reversed, err := capturedFilesystemIdentity(t.Context(), records); err != nil || reversed != baseline {
		t.Fatalf("record order changes captured identity: %s %v", reversed, err)
	}
	for name, mutate := range map[string]func([]CapturedEntry){
		"root mtime": func(r []CapturedEntry) { r[0].Header.ModTime = r[0].Header.ModTime.Add(time.Nanosecond) },
		"root mode":  func(r []CapturedEntry) { r[0].Header.Mode ^= 0100 },
		"root owner": func(r []CapturedEntry) { r[0].Header.Uid++ },
		"root binary xattr": func(r []CapturedEntry) {
			r[0].Header.PAXRecords = map[string]string{"SCHILY.xattr.user.root": "\xff\x00"}
		},
		"directory mtime": func(r []CapturedEntry) { r[1].Header.ModTime = r[1].Header.ModTime.Add(time.Second) },
		"payload":         func(r []CapturedEntry) { r[3].Content = digest.FromString("other") },
		"file binary xattr": func(r []CapturedEntry) {
			r[3].Header.PAXRecords = maps.Clone(r[3].Header.PAXRecords)
			r[3].Header.PAXRecords["SCHILY.xattr.user.binary"] = "\x00\xfe\x80"
			r[4].Header.PAXRecords = maps.Clone(r[3].Header.PAXRecords)
		},
		"symlink": func(r []CapturedEntry) { r[5].Header.Linkname = "../a/other" },
		"hardlink split": func(r []CapturedEntry) {
			r[4].Header.Typeflag, r[4].Header.Linkname, r[4].Header.Size = tar.TypeReg, "", r[3].Header.Size
			r[4].Content = r[3].Content
		},
		"binary filename": func(r []CapturedEntry) { r[5].Header.Name = "b/\xff" },
	} {
		t.Run(name, func(t *testing.T) {
			changed := capturedRecords()
			mutate(changed)
			got, err := capturedFilesystemIdentity(t.Context(), changed)
			if err != nil || got == baseline {
				t.Fatalf("metadata change did not change captured identity: %s %v", got, err)
			}
		})
	}
}

func TestCapturedFilesystemRejectsIncompleteRecords(t *testing.T) {
	for name, mutate := range map[string]func([]CapturedEntry) []CapturedEntry{
		"missing root":      func(r []CapturedEntry) []CapturedEntry { return r[1:] },
		"missing directory": func(r []CapturedEntry) []CapturedEntry { return append(r[:1], r[2:]...) },
		"missing metadata":  func(r []CapturedEntry) []CapturedEntry { r[1].Header = nil; return r },
		"duplicate":         func(r []CapturedEntry) []CapturedEntry { return append(r, r[1]) },
		"missing digest":    func(r []CapturedEntry) []CapturedEntry { r[3].Content = ""; return r },
		"negative size":     func(r []CapturedEntry) []CapturedEntry { r[3].Header.Size = -1; return r },
		"malformed digest":  func(r []CapturedEntry) []CapturedEntry { r[3].Content = "sha256"; return r },
		"directory payload": func(r []CapturedEntry) []CapturedEntry {
			r[1].Content = digest.FromString("not a file")
			return r
		},
		"escaping path": func(r []CapturedEntry) []CapturedEntry { r[5].Header.Name = "../outside"; return r },
		"conflicting empty hardlink xattrs": func(r []CapturedEntry) []CapturedEntry {
			r[3].Header.PAXRecords = map[string]string{"SCHILY.xattr.user.a": ""}
			r[4].Header.PAXRecords = map[string]string{"SCHILY.xattr.user.b": ""}
			return r
		},
		"missing link target": func(r []CapturedEntry) []CapturedEntry {
			r[4].Header.Linkname = "a/missing"
			return r
		},
		"access time": func(r []CapturedEntry) []CapturedEntry { r[3].Header.AccessTime = time.Now(); return r },
	} {
		t.Run(name, func(t *testing.T) {
			if got, err := capturedFilesystemIdentity(t.Context(), mutate(capturedRecords())); err == nil {
				t.Fatalf("incomplete records accepted: %s", got)
			}
		})
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := capturedFilesystemIdentity(ctx, capturedRecords()); err == nil {
		t.Fatal("canceled capture identity succeeded")
	}
}
