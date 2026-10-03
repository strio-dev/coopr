package definition

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestDocumentationKDLExamples(t *testing.T) {
	// Package-only Nix builds omit Markdown; docs-check enables this repository check.
	if os.Getenv("COOPR_TEST_DOCS") != "1" {
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
				if _, err := Parse(strings.NewReader(string(match[1]))); err != nil {
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
