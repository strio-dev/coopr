package definition

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParsePreservesValuesAcrossStreamingBufferBoundary(t *testing.T) {
	const bufferSize = 64 * 1024
	value := strings.Repeat("v", 160)
	readers := []struct {
		name string
		open func(*testing.T, string) io.Reader
	}{
		{"strings.Reader", func(_ *testing.T, source string) io.Reader { return strings.NewReader(source) }},
		{"bytes.Reader", func(_ *testing.T, source string) io.Reader { return bytes.NewReader([]byte(source)) }},
		{"bytes.Buffer", func(_ *testing.T, source string) io.Reader { return bytes.NewBufferString(source) }},
		{"os.File", func(t *testing.T, source string) io.Reader {
			path := filepath.Join(t.TempDir(), "boundary.coopr")
			if err := os.WriteFile(path, []byte(source), 0600); err != nil {
				t.Fatal(err)
			}
			file, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := file.Close(); err != nil {
					t.Error(err)
				}
			})
			return file
		}},
	}
	for _, boundary := range []struct {
		name   string
		offset int
	}{
		{"property-name", 8},
		{"property-value", 40},
	} {
		t.Run(boundary.name, func(t *testing.T) {
			prefixSize := bufferSize - boundary.offset
			// Short tokens exercise normal buffer refills instead of growing the
			// scanner buffer to accommodate one oversized prefix token.
			source := strings.Repeat("//x\n", prefixSize/4) + strings.Repeat(" ", prefixSize%4) +
				`env "ORIGINAL"="` + value + "\"\n" + strings.Repeat("// tail\n", 20000)
			for _, reader := range readers {
				t.Run(reader.name, func(t *testing.T) {
					def, err := Parse(reader.open(t, source))
					if err != nil {
						t.Fatal(err)
					}
					if len(def.Instructions) != 1 {
						t.Fatalf("instruction count = %d, want 1", len(def.Instructions))
					}
					inst := def.Instructions[0]
					if inst.Name != "env" || len(inst.Arguments) != 0 || len(inst.Properties) != 1 || inst.Properties["ORIGINAL"] != value {
						t.Fatalf("parsed instruction = %#v, want ENV ORIGINAL=%q", inst, value)
					}
				})
			}
		})
	}
}
