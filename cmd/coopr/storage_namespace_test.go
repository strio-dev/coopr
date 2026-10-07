package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
)

func TestNativeCommandsPrepareStorageNamespace(t *testing.T) {
	for _, args := range [][]string{
		{"build"}, {"component", "build"}, {"copy", "source", "local:destination"},
		{"images"}, {"image", "ls"}, {"image", "inspect", "image"}, {"image", "rm", "image"},
		{"system", "df"}, {"system", "prune"}, {"image", "prune"},
	} {
		t.Run(args[0]+"/"+args[len(args)-1], func(t *testing.T) {
			want := errors.New("namespace initialization failed")
			calls := 0
			root := newRootCommandWithStorageNamespace(func() error {
				calls++
				return want
			})
			cmd, _, err := root.Find(args)
			if err != nil {
				t.Fatal(err)
			}
			cmd.RunE = func(*cobra.Command, []string) error {
				t.Fatal("native command ran after namespace initialization failed")
				return nil
			}
			root.SetArgs(args)
			if err := root.Execute(); !errors.Is(err, want) || calls != 1 {
				t.Fatalf("namespace calls=%d error=%v, want one call and %v", calls, err, want)
			}
		})
	}
}

func TestStorageNamespacePrecedesNativeDefaults(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, "storage.conf")
	t.Setenv("CONTAINERS_STORAGE_CONF", config)
	wantGraph, wantRun := filepath.Join(dir, "graph"), filepath.Join(dir, "run")
	calls := 0
	root := newRootCommandWithStorageNamespace(func() error {
		calls++
		return os.WriteFile(config, []byte("[storage]\ndriver = \"vfs\"\ngraphroot = \""+wantGraph+"\"\nrunroot = \""+wantRun+"\"\n"), 0o600)
	})
	cmd, _, err := root.Find([]string{"images"})
	if err != nil {
		t.Fatal(err)
	}
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		store, err := commandStorage(cmd)
		if err != nil {
			return err
		}
		if calls != 1 || store.GraphRoot != wantGraph || store.RunRoot != wantRun || store.GraphDriverName != "vfs" {
			t.Fatalf("namespace calls=%d store=%+v", calls, store)
		}
		return nil
	}
	root.SetArgs([]string{"images"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
}

func TestArtifactCommandsDoNotPrepareStorageNamespace(t *testing.T) {
	config := filepath.Join(t.TempDir(), "storage.conf")
	if err := os.WriteFile(config, []byte("[storage\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONTAINERS_STORAGE_CONF", config)
	for _, args := range [][]string{
		{"components"}, {"component", "ls"}, {"component", "inspect", "component"},
		{"component", "rm", "component"}, {"component", "copy", "source", "local:destination"},
	} {
		t.Run(args[len(args)-1], func(t *testing.T) {
			root := newRootCommandWithStorageNamespace(func() error {
				t.Fatal("artifact command attempted namespace initialization")
				return nil
			})
			cmd, _, err := root.Find(args)
			if err != nil {
				t.Fatal(err)
			}
			cmd.RunE = func(*cobra.Command, []string) error { return nil }
			root.SetArgs(args)
			if err := root.Execute(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestInformationalCommandsDoNotPrepareStorageNamespace(t *testing.T) {
	config := filepath.Join(t.TempDir(), "storage.conf")
	if err := os.WriteFile(config, []byte("[storage\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONTAINERS_STORAGE_CONF", config)
	for _, args := range [][]string{
		nil, {"--help"}, {"--version"}, {"build", "--help"}, {"component", "build", "--help"},
		{"image"}, {"component"}, {"system"}, {"image", "prune", "--help"}, {"system", "prune", "--help"}, {"help", "images"}, {"completion", "bash"},
	} {
		var stdout, stderr bytes.Buffer
		status := runContextWithStorageNamespace(t.Context(), args, &stdout, &stderr, func() error {
			t.Fatalf("%v attempted namespace initialization", args)
			return nil
		})
		if status != 0 {
			t.Fatalf("%v: status=%d stderr=%s", args, status, &stderr)
		}
	}
}

func TestNativeCommandsRejectInvalidStorageConfiguration(t *testing.T) {
	config := filepath.Join(t.TempDir(), "storage.conf")
	if err := os.WriteFile(config, []byte("[storage\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONTAINERS_STORAGE_CONF", config)
	for _, args := range [][]string{{"images"}, {"image", "inspect", "image"}, {"build"}, {"component", "build"}, {"copy", "source", "local:destination"}, {"system", "df"}, {"image", "prune"}} {
		root := newRootCommand()
		cmd, _, err := root.Find(args)
		if err != nil {
			t.Fatal(err)
		}
		cmd.RunE = func(*cobra.Command, []string) error {
			t.Fatalf("%v ran with invalid storage configuration", args)
			return nil
		}
		root.SetArgs(args)
		if err := root.Execute(); err == nil {
			t.Fatalf("%v accepted invalid storage configuration", args)
		}
	}
}

func TestNamespaceErrorPrecedesStorageConfiguration(t *testing.T) {
	config := filepath.Join(t.TempDir(), "storage.conf")
	if err := os.WriteFile(config, []byte("invalid TOML ["), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONTAINERS_STORAGE_CONF", config)
	want := errors.New("namespace initialization failed")
	root := newRootCommandWithStorageNamespace(func() error { return want })
	root.SetArgs([]string{"images"})
	if err := root.Execute(); !errors.Is(err, want) {
		t.Fatalf("error=%v, want namespace failure before storage configuration", err)
	}
}
