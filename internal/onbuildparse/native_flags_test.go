package onbuildparse

import (
	"strings"
	"testing"
)

func TestNativeRunFlagsAndChildScope(t *testing.T) {
	for _, raw := range []string{
		"RUN --mount=type=cache,target=/cache,nosuid,nodev,noexec,U true",
		"RUN --network=$NETWORK --mount=type=bind,target=/src,bind-nonrecursive,Z true",
	} {
		if _, err := ParseDeferred(raw); err != nil {
			t.Fatalf("deferred %q: %v", raw, err)
		}
		got, err := ParseExpanded(raw, func(word string) (string, error) { return strings.ReplaceAll(word, "$NETWORK", "none"), nil })
		if err != nil {
			t.Fatalf("resolved %q: %v", raw, err)
		}
		if len(got[0].Children) != 1 {
			t.Fatalf("mounts: %#v", got)
		}
	}
}
func TestNativeCopyFlagChildScope(t *testing.T) {
	raw := "COPY --link=$LINK --parents=$PARENTS src /dst/"
	if _, err := ParseDeferred(raw); err != nil {
		t.Fatal(err)
	}
	got, err := ParseExpanded(raw, func(word string) (string, error) {
		return strings.NewReplacer("$LINK", "true", "$PARENTS", "true").Replace(word), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Properties["link"] != "true" || got[0].Properties["parents"] != "true" {
		t.Fatalf("flags: %#v", got)
	}
}

func TestNativeMountRejectsInvalidSourceOrderedFields(t *testing.T) {
	for _, flags := range []string{"Z=garbage", "nosuid=true", "U=false", "bind-propagation=bad,bind-propagation=private", "relabel=bad,relabel=private", "relabel=,relabel=private", "bind-propagation=,bind-propagation=private", "relabel=private,relabel=shared", "bind-propagation=$literal,bind-propagation=private"} {
		raw := "RUN --mount=type=bind,target=/src," + flags + " true"
		if _, err := ParseExpanded(raw, func(word string) (string, error) { return word, nil }); err == nil {
			t.Errorf("accepted %s", raw)
		}
	}
}

func TestNativeMountRejectsTypeInappropriateFieldSpelling(t *testing.T) {
	for _, flags := range []string{
		"type=tmpfs,target=/tmp,mode=0700", "type=tmpfs,target=/tmp,size=1m",
		"type=cache,target=/cache,relabel=private", "type=cache,target=/cache,tmpfs-mode=0700",
	} {
		if _, err := ParseExpanded("RUN --mount="+flags+" true", func(word string) (string, error) { return word, nil }); err == nil {
			t.Errorf("accepted %s", flags)
		}
	}
}

func TestImportedOnBuildMaintainerRemainsLiteral(t *testing.T) {
	got, err := ParseExpanded("MAINTAINER $later", func(word string) (string, error) { return strings.ReplaceAll(word, "$later", "expanded"), nil })
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Name != "maintainer" || got[0].Arguments[0] != "$later" {
		t.Fatalf("imported maintainer: %#v", got)
	}
}

func TestDynamicMountTypePreservesNativeSpellingUntilResolution(t *testing.T) {
	for _, test := range []struct {
		field, kind string
		valid       bool
	}{
		{"mode=0700", "cache", true}, {"mode=0700", "tmpfs", false},
		{"tmpfs-mode=0700", "tmpfs", true}, {"tmpfs-mode=0700", "cache", false},
		{"source=payload", "bind", true}, {"source=payload", "secret", false},
		{"ro", "tmpfs", true}, {"rw", "tmpfs", false},
	} {
		raw := "RUN --mount=type=$kind,target=/target," + test.field + " true"
		deferred, err := ParseDeferred(raw)
		if err != nil {
			t.Fatalf("defer %s: %v", raw, err)
		}
		if deferred[0].DeferredOnBuild != raw {
			t.Fatalf("native field identity lost: %#v", deferred)
		}
		_, err = ParseExpanded(raw, func(word string) (string, error) { return strings.ReplaceAll(word, "$kind", test.kind), nil })
		if (err == nil) != test.valid {
			t.Errorf("%s type %s: %v", raw, test.kind, err)
		}
	}
}
