package main

import (
	"os"
	"path/filepath"
	"testing"

	"coopr/internal/oci"
	"github.com/spf13/cobra"
	commonconfig "go.podman.io/common/pkg/config"
)

func TestRegistryFlagsUseNativeAuthDefault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	t.Setenv("REGISTRY_AUTH_FILE", path)
	var flags registryFlags
	cmd := &cobra.Command{}
	flags.addTo(cmd)
	if got := flags.resolverOptions(cmd).AuthFile; got != path {
		t.Fatalf("authfile=%q, want %q", got, path)
	}
}

func TestRootModuleAppliesBeforeRegistryOperation(t *testing.T) {
	module := filepath.Join(t.TempDir(), "registry.conf")
	if err := os.WriteFile(module, []byte("[engine]\nretry=7\nretry_delay='13ms'\ncompression_format='zstd'\ncompression_level=3\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = commonconfig.New(&commonconfig.Options{SetDefault: true}) })
	root := newRootCommand()
	probe := &cobra.Command{Use: "registry-policy-probe", RunE: func(cmd *cobra.Command, _ []string) error {
		retry, err := oci.RegistryRetryOptions(oci.Options{})
		if err != nil {
			return err
		}
		if retry.MaxRetry != 7 || retry.Delay.String() != "13ms" {
			t.Fatalf("retry=%+v", retry)
		}
		return nil
	}}
	root.AddCommand(probe)
	root.SetArgs([]string{"--module", module, "registry-policy-probe"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
}

func TestRootResolvesCredentialsBeforeRegistryOperation(t *testing.T) {
	root := newRootCommand()
	var registry registryFlags
	called := false
	probe := &cobra.Command{Use: "registry-creds-probe", RunE: func(cmd *cobra.Command, _ []string) error {
		called = true
		if registry.credentials != "tester:secret" {
			t.Fatalf("credentials not retained")
		}
		return nil
	}}
	registry.addTo(probe)
	root.AddCommand(probe)
	root.SetArgs([]string{"registry-creds-probe", "--creds=tester:secret"})
	if err := root.Execute(); err != nil || !called {
		t.Fatalf("complete credentials: %v, called=%v", err, called)
	}
	// Native AuthConfig must report a terminal error rather than use an empty password.
	old := os.Stdin
	input, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	os.Stdin = input
	t.Cleanup(func() { os.Stdin = old; _ = input.Close() })
	called = false
	root.SetArgs([]string{"registry-creds-probe", "--creds=tester"})
	if err := root.Execute(); err == nil || called {
		t.Fatalf("missing password: %v, called=%v", err, called)
	}
}
