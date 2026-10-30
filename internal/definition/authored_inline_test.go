package definition_test

import (
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/strio-dev/coopr/internal/definition"
	"github.com/strio-dev/coopr/internal/onbuildparse"
)

func TestAuthoredInlineCopyAddExactBytes(t *testing.T) {
	for _, kind := range []string{"copy", "add"} {
		t.Run(kind, func(t *testing.T) {
			parsed, err := parseFragment(strings.NewReader(kind + ` "ordinary" "/out/" chown="12:34" chmod=0o640 link=#true parents=#true {
 file "normal" "Hello ${name}\n\n"
 file "raw" #"Hello ${name}\n"#
 file "multi" """
     first
     second
     """
 file "rawmulti" #"""
     ${literal}
     """#
 file "empty" ""
 file "nul" "nul\u{0}exact"
 file "normal" "last"
 exclude "normal"
}`))
			if err != nil {
				t.Fatal(err)
			}
			inst := parsed.Instructions[0]
			want := []definition.InlineFile{{Path: "normal", Data: "Hello ${name}\n\n", Expand: true}, {Path: "raw", Data: `Hello ${name}\n`}, {Path: "multi", Data: "first\nsecond", Expand: true}, {Path: "rawmulti", Data: "${literal}"}, {Path: "empty", Expand: true}, {Path: "nul", Data: "nul\x00exact", Expand: true}, {Path: "normal", Data: "last", Expand: true}}
			if !reflect.DeepEqual(inst.InlineFiles, want) {
				t.Fatalf("files = %#v, want %#v", inst.InlineFiles, want)
			}
			if len(inst.Children) != 1 || inst.Children[0].Name != "exclude" {
				t.Fatalf("children = %#v", inst.Children)
			}
		})
	}
}

func TestAuthoredInlineRejectsInvalidFiles(t *testing.T) {
	for _, source := range []string{
		`copy "/out/" { file "name" }`, `copy "/out/" { file "name" 12 }`,
		`copy "/out/" { file "name" "data" extra=#true }`, `copy "/out/" { file "name" "data" {} }`,
		`copy "/out/" { file "../escape" "data" }`, `copy "/out/" { file "/absolute" "data" }`,
		`copy "/out/" { file "." "data" }`, `copy "/out/" { file "sub/../../escape" "data" }`, `copy "/out/" { file "sub/.." "data" }`, `copy "/out/" { file "" "data" }`, `copy "/out/" { file "bad\u{0}name" "data" }`,
		`run "true" { file "name" "data" }`,
		`file "name" "data"`, `copy "/out/" { file (tag)"name" "data" }`,
	} {
		if _, err := parseFragment(strings.NewReader(source)); err == nil {
			t.Errorf("accepted %s", source)
		}
	}
}

func TestAuthoredInlineOnBuildRoundTrip(t *testing.T) {
	parsed, err := parseFragment(strings.NewReader(`onbuild { copy "ordinary" "/out/" { file "normal" "Hello ${name}\n\n"; file "raw" #"""
${literal}

"""#; } }`))
	if err != nil {
		t.Fatal(err)
	}
	trigger := parsed.Instructions[0].Arguments[0]
	got, err := onbuildparse.ParseDeferred(trigger)
	if err != nil {
		t.Fatal(err)
	}
	want := []definition.InlineFile{{Path: "normal", Data: "Hello ${name}\n\n", Expand: true}, {Path: "raw", Data: "${literal}\n"}}
	if !reflect.DeepEqual(got[0].InlineFiles, want) {
		t.Fatalf("trigger %q files = %#v, want %#v", trigger, got[0].InlineFiles, want)
	}
}

func TestAuthoredInlineOnBuildRejectsLossyMetadata(t *testing.T) {
	for _, child := range []string{
		`file "name" ""`, `file "name" "unterminated"`, `file "name" "name\n"`, `file "name" "CR\r\n"`,
		`file "bad\nname" "ok\n"`, `file "same" "first\n"; file "same" "second\n"`,
	} {
		if _, err := parseFragment(strings.NewReader(`onbuild { copy "/out/" { ` + child + ` } }`)); err == nil {
			t.Errorf("accepted lossy %s", child)
		}
	}
}

func TestAuthoredInlineOnBuildPreservesLiteralNames(t *testing.T) {
	for _, name := range []string{"${filename}", "bad name", "-leading", "sub/name", "a'b", `a\b`, `a"b`} {
		for _, raw := range []bool{false, true} {
			body := `"ok\n"`
			if raw {
				body = `#"""
ok

"""#`
			}
			source := `onbuild { copy "/out/" { file ` + strconv.Quote(name) + ` ` + body + `; } }`
			parsed, err := parseFragment(strings.NewReader(source))
			if name == "bad name" {
				if err == nil {
					t.Fatal("accepted native filename truncation")
				}
				continue
			}
			if err != nil {
				t.Fatalf("%s: %v", source, err)
			}
			files, err := onbuildparse.ParseDeferred(parsed.Instructions[0].Arguments[0])
			if err != nil {
				t.Fatal(err)
			}
			if got := files[0].InlineFiles[0]; got.Path != name || got.Expand == raw || got.Data != "ok\n" {
				t.Fatalf("name %q raw %t = %#v", name, raw, got)
			}
		}
	}
}

func TestAuthoredInlineOnBuildRejectsLossyHeader(t *testing.T) {
	for _, args := range []string{`"has space" "/out/"`, `"source" "/out space/"`, `"has\tspace" "/out/"`} {
		if _, err := parseFragment(strings.NewReader(`onbuild { copy ` + args + ` { file "inline" "ok\n"; }; }`)); err == nil {
			t.Errorf("accepted lossy mixed header: %s", args)
		}
	}
}

func TestAuthoredInlineOnBuildPreservesQuotedHeader(t *testing.T) {
	for _, kind := range []string{"copy", "add"} {
		for _, source := range []struct{ word, value string }{
			{"ordinary", "ordinary"}, {`has"quote`, `has"quote`}, {"has'quote", "has'quote"}, {`back\slash`, "backslash"},
		} {
			for _, destination := range []struct{ word, value string }{
				{"/out/", "/out/"}, {`/out"quote/`, `/out"quote/`}, {`/back\slash/`, "/backslash/"},
			} {
				t.Run(kind+"/"+source.word+"/"+destination.word, func(t *testing.T) {
					authored := `onbuild { ` + kind + ` ` + strconv.Quote(source.word) + ` ` + strconv.Quote(destination.word) + ` { file "inline" "Hello ${name}\n\n"; file "raw" #"""
${literal}

"""#; }; }`
					parsed, err := parseFragment(strings.NewReader(authored))
					if err != nil {
						t.Fatal(err)
					}
					trigger := parsed.Instructions[0].Arguments[0]
					got, err := onbuildparse.Parse(trigger)
					if err != nil {
						t.Fatal(err)
					}
					wantFiles := []definition.InlineFile{{Path: "inline", Data: "Hello ${name}\n\n", Expand: true}, {Path: "raw", Data: "${literal}\n"}}
					deferred, err := onbuildparse.ParseDeferred(trigger)
					if err != nil {
						t.Fatal(err)
					}
					if len(got) != 1 || got[0].Name != kind || !reflect.DeepEqual(got[0].Arguments, []string{source.value, destination.value}) || len(deferred) != 1 || !reflect.DeepEqual(deferred[0].InlineFiles, wantFiles) {
						t.Fatalf("trigger %q parsed=%#v deferred=%#v", trigger, got, deferred)
					}
				})
			}
		}
	}
}

func TestAuthoredInlineOnBuildPreservesNULAndEmbeddedCRContent(t *testing.T) {
	parsed, err := parseFragment(strings.NewReader(`onbuild { copy "/out/" { file "nul" "nul\u{0}exact\n"; file "cr" "a\rb\n"; }; }`))
	if err != nil {
		t.Fatal(err)
	}
	files, err := onbuildparse.ParseDeferred(parsed.Instructions[0].Arguments[0])
	if err != nil {
		t.Fatal(err)
	}
	if got := files[0].InlineFiles[0].Data; got != "nul\x00exact\n" {
		t.Fatalf("NUL content = %q", got)
	}
	if got := files[0].InlineFiles[1].Data; got != "a\rb\n" {
		t.Fatalf("embedded CR content = %q", got)
	}
}
