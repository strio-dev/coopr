package buildah

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"coopr/internal/definition"
	upstream "go.podman.io/buildah"
)

type inlineParityBuilder struct {
	recordingBuilder
	names    []string
	payloads []string
}

func (builder *inlineParityBuilder) add(destination string, extract bool, options upstream.AddAndCopyOptions, sources ...string) error {
	for _, source := range sources {
		builder.names = append(builder.names, source)
		data, err := os.ReadFile(filepath.Join(options.ContextDir, source))
		if err != nil {
			return err
		}
		builder.payloads = append(builder.payloads, string(data))
	}
	return builder.recordingBuilder.add(destination, extract, options, sources...)
}

func TestInlineCopyAddUsesNativeOrderAndIgnoresExclusions(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "same"), []byte("ordinary"), 0600); err != nil {
		t.Fatal(err)
	}
	inline := []definition.InlineFile{{Path: "same", Data: "inline\n\x00exact"}, {Path: "second", Data: "second"}}
	for _, kind := range []string{"copy", "add"} {
		t.Run(kind, func(t *testing.T) {
			builder := &inlineParityBuilder{}
			var operation Operation = Copy{Sources: []string{"same"}, InlineFiles: inline, Destination: "/out/", Excludes: []string{"second"}, Chown: "12:34", Chmod: "0640", Link: true, Parents: true}
			if kind == "add" {
				operation = Add{Sources: []string{"same"}, InlineFiles: inline, Destination: "/out/", Excludes: []string{"second"}, Chown: "12:34", Chmod: "0640", Link: true, Parents: true}
			}
			if err := operation.apply(builder, root); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(builder.payloads, []string{"inline\n\x00exact", "second", "ordinary"}) {
				t.Fatalf("payload order = %q", builder.payloads)
			}
			for index, options := range builder.addOptions[:1] {
				if len(options.Excludes) != 0 || options.IgnoreFile != "" {
					t.Errorf("inline %d exclusions = %v, ignore = %q", index, options.Excludes, options.IgnoreFile)
				}
				if options.Chown != "12:34" || options.Chmod != "0640" || !options.Link {
					t.Errorf("inline %d options lost: %+v", index, options)
				}
			}
			if !builder.addOptions[1].Parents || len(builder.addOptions[1].Excludes) == 0 {
				t.Fatal("ordinary-source options lost")
			}
		})
	}
}

func TestAddInlineDataBatchesDuplicateSourceNames(t *testing.T) {
	builder := &inlineParityBuilder{}
	sources := []definition.InlineFile{{Path: "same", Data: "first\n\x00"}, {Path: "same", Data: "second"}}
	if err := addInlineData(builder, "/out/", sources, upstream.AddAndCopyOptions{Link: true}); err != nil {
		t.Fatal(err)
	}
	if len(builder.addOptions) != 1 {
		t.Fatalf("native Add calls = %d, want one", len(builder.addOptions))
	}
	if !reflect.DeepEqual(builder.payloads, []string{"first\n\x00", "second"}) {
		t.Fatalf("duplicate source payloads = %q", builder.payloads)
	}
}
