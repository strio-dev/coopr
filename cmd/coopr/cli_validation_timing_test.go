package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/strio-dev/coopr/internal/oci"

	"github.com/spf13/cobra"
	commonconfig "go.podman.io/common/pkg/config"
)

func TestInvalidRegistryValuesDoNotPreventHelp(t *testing.T) {
	for _, args := range [][]string{{"build", "--pull=bad"}, {"build", "--retry=-1"}, {"build", "--retry-delay=bad"}, {"pull", "--retry-delay=bad"}, {"push", "--retry-delay=bad"}} {
		var out, errs bytes.Buffer
		status := run(append(args, "--help"), &out, &errs)
		if status != 0 || !strings.Contains(out.String(), "Usage:") || errs.Len() != 0 {
			t.Fatalf("%v status=%d out=%s err=%s", args, status, &out, &errs)
		}
	}
}

func TestImageSortRejectsInvalidValuesBeforeHelp(t *testing.T) {
	for _, path := range [][]string{{"images"}, {"image", "ls"}, {"image", "list"}} {
		for _, sort := range []string{"", "a,b", "CREATED"} {
			var out, errs bytes.Buffer
			args := append(append([]string{}, path...), "--sort="+sort, "--help")
			if status := run(args, &out, &errs); status != 125 || out.Len() != 0 || !strings.Contains(errs.String(), "not a valid value") {
				t.Fatalf("%v status=%d out=%s err=%s", args, status, &out, &errs)
			}
		}
	}
}

func TestRegistryRuntimeValidation(t *testing.T) {
	for _, arg := range []string{"--retry=-1", "--retry-delay=bad"} {
		var flags registryFlags
		called := false
		cmd := &cobra.Command{Use: "probe", RunE: func(*cobra.Command, []string) error { called = true; return nil }}
		flags.addTo(cmd)
		cmd.SetArgs([]string{arg})
		if err := cmd.Execute(); err == nil || called {
			t.Fatalf("%s err=%v called=%t", arg, err, called)
		}
	}
	var flags registryFlags
	cmd := &cobra.Command{Use: "probe", RunE: func(*cobra.Command, []string) error { return nil }}
	flags.addTo(cmd)
	cmd.SetArgs([]string{"--retry=0", "--retry-delay=13ms"})
	if err := cmd.Execute(); err != nil || flags.retry != 0 || flags.retryDelay.String() != "13ms" {
		t.Fatalf("retry=%d delay=%s err=%v", flags.retry, flags.retryDelay, err)
	}
	cmd.SetArgs([]string{"--retry=2", "--retry-delay="})
	if err := cmd.Execute(); err != nil || flags.retry != 2 || flags.retryDelay != 0 {
		t.Fatalf("reused command retry=%d delay=%s err=%v", flags.retry, flags.retryDelay, err)
	}
}

func TestRegistryDelayUsesSelectedModuleWhenOmitted(t *testing.T) {
	initial, selected := filepath.Join(t.TempDir(), "initial.conf"), filepath.Join(t.TempDir(), "selected.conf")
	for path, delay := range map[string]string{initial: "11ms", selected: "13ms"} {
		if err := os.WriteFile(path, []byte("[engine]\nretry=7\nretry_delay='"+delay+"'\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { _, _ = commonconfig.New(&commonconfig.Options{SetDefault: true}) })
	if _, err := commonconfig.New(&commonconfig.Options{Modules: []string{initial}, SetDefault: true}); err != nil {
		t.Fatal(err)
	}
	root := newRootCommand()
	var registry registryFlags
	probe := &cobra.Command{Use: "registry-delay-probe", RunE: func(cmd *cobra.Command, _ []string) error {
		retry, err := oci.RegistryRetryOptions(registry.resolverOptions(cmd))
		if err != nil {
			return err
		}
		if retry.MaxRetry != 7 || retry.Delay.String() != "13ms" {
			t.Fatalf("selected module retry=%+v", retry)
		}
		return nil
	}}
	registry.addTo(probe)
	root.AddCommand(probe)
	root.SetArgs([]string{"--module", selected, "registry-delay-probe"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
}

func TestEveryRegistryCommandValidatesAtExecution(t *testing.T) {
	for _, test := range []struct {
		factory func() *cobra.Command
		args    []string
	}{
		{newBuildCommand, nil}, {newComponentBuildCommand, nil},
		{newImagePullCommand, []string{"unused"}}, {newImagePushCommand, []string{"unused"}},
		{newComponentPullCommand, []string{"unused"}}, {newComponentPushCommand, []string{"unused", "unused"}},
		{newManifestCreateCommand, []string{"unused"}}, {newManifestAddCommand, []string{"unused", "unused"}},
		{newManifestInspectCommand, []string{"unused"}}, {newManifestPushCommand, []string{"unused"}},
	} {
		for _, option := range []string{"--retry=-1", "--retry-delay=bad"} {
			cmd := test.factory()
			var out, errs bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&errs)
			cmd.SetArgs(append(append([]string{}, test.args...), option))
			if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), strings.Split(option, "=")[0]) {
				t.Fatalf("%s %s execution err=%v", cmd.Use, option, err)
			}
		}
	}
}
