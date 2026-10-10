package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCompletionFileAndDescriptions(t *testing.T) {
	for _, shell := range []string{"bash", "zsh", "fish", "powershell"} {
		t.Run(shell, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "completion")
			var out, errs bytes.Buffer
			if status := run([]string{"completion", shell, "--file", path, "--no-desc"}, &out, &errs); status != 0 || out.Len() != 0 || errs.Len() != 0 {
				t.Fatalf("status=%d out=%q err=%q", status, out.String(), errs.String())
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(data), "coopr") {
				t.Fatalf("missing completion: %s", data)
			}
		})
	}
}

func TestImageConvenienceAliases(t *testing.T) {
	root := newRootCommand()
	for _, args := range [][]string{{"rmi"}, {"inspect"}, {"image", "list"}} {
		cmd, rest, err := root.Find(args)
		if err != nil || len(rest) != 0 || cmd == root {
			t.Fatalf("alias %v: cmd=%v rest=%v err=%v", args, cmd, rest, err)
		}
	}
	info, _, _ := root.Find([]string{"info"})
	if flag := info.Flags().ShorthandLookup("f"); flag == nil || flag.Name != "format" {
		t.Fatalf("info -f missing: %v", flag)
	}
}
