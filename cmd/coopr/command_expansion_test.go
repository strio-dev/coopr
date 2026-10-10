package main

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

func TestImageManagementRootAliases(t *testing.T) {
	for _, name := range []string{"pull", "push", "tag", "save", "load", "exists", "history"} {
		root := newRootCommand()
		command, _, err := root.Find([]string{name})
		if err != nil || command.Name() != name {
			t.Fatalf("missing root command %s: %v", name, err)
		}
		imageCommand, _, err := root.Find([]string{"image", name})
		if err != nil || imageCommand.Name() != name || command.Use != imageCommand.Use {
			t.Fatalf("root/image %s differ: %v", name, err)
		}
	}
}

func TestExistsReportsNamespaceAndArgumentErrorsAs125(t *testing.T) {
	for _, args := range [][]string{{"exists"}, {"image", "exists"}, {"manifest", "exists"}, {"component", "exists"}} {
		var out, errs bytes.Buffer
		if status := run(args, &out, &errs); status != 125 || errs.Len() == 0 {
			t.Fatalf("%v status=%d stderr=%q", args, status, &errs)
		}
	}
	var out, errs bytes.Buffer
	if status := runContextWithStorageNamespace(context.Background(), []string{"exists", "anything"}, &out, &errs, func() error {
		return errors.New("namespace unavailable")
	}); status != 125 || errs.String() != "Error: namespace unavailable\n" {
		t.Fatalf("namespace failure status=%d stderr=%q", status, &errs)
	}
}
