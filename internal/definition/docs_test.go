package definition

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	kdl "github.com/calico32/kdl-go"
)

func TestDocumentationKDLExamples(t *testing.T) {
	// Package-only Nix builds omit Markdown; docs-check runs this repository check.
	if testing.Short() {
		t.Skip("run just docs-check to validate the public Markdown examples")
	}
	paths := []string{"../../README.md", "../../CONTRIBUTING.md"}
	for _, root := range []string{"../../docs", "../../examples"} {
		if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !entry.IsDir() && filepath.Ext(path) == ".md" {
				paths = append(paths, path)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	fence := regexp.MustCompile("(?ms)^```kdl[ \\t]*\\r?\\n(.*?)^```[ \\t]*\\r?$")
	count := 0
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for i, match := range fence.FindAllSubmatch(data, -1) {
			count++
			t.Run(fmt.Sprintf("%s/block%d", path, i+1), func(t *testing.T) {
				source := string(match[1])
				doc, err := kdl.ParseString(source, kdl.WithVersion(kdl.Version2))
				if err != nil {
					t.Fatal(err)
				}
				// Reference pages show operation fragments as well as complete
				// definitions. Validate fragments inside a canonical stage; a
				// source containing any stage head must stand on its own.
				parse := parseFragment
				for _, node := range doc.Nodes {
					if isStage(node.Name()) {
						parse = Parse
						break
					}
				}
				if _, err := parse(strings.NewReader(source)); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
	if count == 0 {
		t.Fatal("no public KDL examples found")
	}
	t.Logf("parsed %d public KDL examples", count)
}
