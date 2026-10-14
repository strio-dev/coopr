package onbuildparse

import (
	"github.com/openshift/imagebuilder"
	"reflect"
	"strings"
	"testing"
)

func TestNativeCopyAddRepeatedFlags(t *testing.T) {
	for _, name := range []string{"COPY", "ADD"} {
		t.Run(name, func(t *testing.T) {
			got, err := Parse(name + ` --chown=1 --chown=2 --chmod=0600 --chmod=0640 --link=true --link=false src /dst`)
			if err != nil {
				t.Fatal(err)
			}
			want := map[string]string{"chown": "2", "chmod": "0640"}
			if !reflect.DeepEqual(got[0].Properties, want) {
				t.Fatalf("properties = %#v, want %#v", got[0].Properties, want)
			}
		})
	}
}

func TestNativeCopyRepeatedParents(t *testing.T) {
	got, err := Parse(`COPY --parents=true --parents=false --parents=true src /dst`)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Properties["parents"] != "true" {
		t.Fatalf("COPY = %#v", got)
	}
}

func TestNativeCopyAddInvalidFlags(t *testing.T) {
	for _, raw := range []string{`COPY --link=TRUE src /dst`, `ADD --link=TRUE src /dst`, `COPY --parents=TRUE src /dst`, `COPY --chown= src /dst`, `ADD --chmod=garbage src /dst`, `COPY --unknown=true src /dst`} {
		t.Run(raw, func(t *testing.T) {
			if _, err := Parse(raw); err == nil {
				t.Fatal("accepted native-invalid flag")
			}
		})
	}
}

func TestNativeCopyAddFlagDifferential(t *testing.T) {
	for _, raw := range []string{
		`COPY --chown=1 --chown=2 --chmod=0600 --chmod=0640 --link=true --link=false --parents=false --parents=true --exclude=one --exclude=two src /dst`,
		`ADD --chown=1 --chown=2 --chmod=0600 --chmod=0640 --link=false --link=true --keep-git-dir=true --keep-git-dir=false --checksum=one --checksum=two src /dst`,
		`COPY --chown="1:2" --chmod="0644" --link="true" src /dst`,
		`COPY --chown='a\"b' --from='c\"d' --exclude='e\"f' src /dst`,
		`COPY --exclude='a\\b' src /dst`,
		`ADD --checksum='a\"b' src /dst`,
		`COPY --chown= --chown=2 src /dst`,
		`COPY --chown="" --chown=2 src /dst`,
		`COPY --chown='""' --chown=2 src /dst`,
		`COPY --from='""' --from=producer src /dst`,
		`COPY --exclude='""' src /dst`,
		`COPY --exclude='""' --exclude=valid src /dst`,
		`ADD --checksum='""' --checksum=valid src /dst`,
		`COPY --chown="a'quote" --chown=2 src /dst`,
		`COPY --from="" --from=producer src /dst`,
		`COPY --exclude="" --exclude=valid src /dst`,
		`COPY --exclude="" src /dst`,
		`COPY --chmod="" --chmod=0644 src /dst`,
		`COPY --link="" --link=true src /dst`,
		`COPY --parents="" --parents=true src /dst`,
		`ADD --checksum="" --checksum=sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef src /dst`,
		`ADD --chown="" --chown=2 src /dst`,
		`ADD --exclude="" --exclude=valid src /dst`,
		`ADD --keep-git-dir="" --keep-git-dir=true src /dst`,
		`COPY --chmod=invalid --chmod=0644 src /dst`,
		`COPY --link=TRUE --link=false src /dst`,
		`ADD --parents src /dst`,
	} {
		t.Run(raw, func(t *testing.T) {
			native, err := imagebuilder.ParseDockerfile(strings.NewReader(raw))
			if err != nil {
				t.Fatal(err)
			}
			builder := imagebuilder.NewBuilder(nil)
			step := builder.Step()
			if err := step.Resolve(native.Children[0]); err != nil {
				t.Fatal(err)
			}
			capture := &nativeCopyCapture{Executor: imagebuilder.NoopExecutor}
			nativeErr := builder.Run(step, capture, true)
			got, err := Parse(raw)
			t.Logf("native error=%v; Coopr error=%v", nativeErr, err)
			if (err != nil) != (nativeErr != nil) {
				t.Fatalf("Coopr error %v, native %v", err, nativeErr)
			}
			if err != nil {
				return
			}
			copy := capture.copies[0]
			properties := got[0].Properties
			for name, want := range map[string]string{"chown": copy.Chown, "chmod": copy.Chmod, "from": copy.From, "checksum": copy.Checksum} {
				if properties[name] != want {
					t.Fatalf("%s = %q, native %q", name, properties[name], want)
				}
			}
			if (properties["link"] == "true") != copy.Link || (properties["parents"] == "true") != copy.Parents || (properties["keep-git-dir"] == "true") != copy.KeepGitDir {
				t.Fatalf("properties %#v, native %#v", properties, copy)
			}
			var excludes []string
			for _, child := range got[0].Children {
				if child.Name == "exclude" {
					excludes = append(excludes, child.Arguments...)
				}
			}
			if !reflect.DeepEqual(excludes, copy.Excludes) {
				t.Fatalf("excludes %#v, native %#v", excludes, copy.Excludes)
			}
		})
	}
}
