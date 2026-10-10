package main

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestRegistryAuthOptionCompletion(t *testing.T) {
	for _, tc := range []struct {
		command   string
		option    string
		directive cobra.ShellCompDirective
	}{
		{"login", "--username", cobra.ShellCompDirectiveNoFileComp},
		{"login", "-u", cobra.ShellCompDirectiveNoFileComp},
		{"login", "--password", cobra.ShellCompDirectiveNoFileComp},
		{"login", "-p", cobra.ShellCompDirectiveNoFileComp},
		{"login", "--authfile", cobra.ShellCompDirectiveDefault},
		{"login", "--compat-auth-file", cobra.ShellCompDirectiveDefault},
		{"login", "--cert-dir", cobra.ShellCompDirectiveDefault},
		{"logout", "--authfile", cobra.ShellCompDirectiveDefault},
		{"logout", "--compat-auth-file", cobra.ShellCompDirectiveDefault},
	} {
		t.Run(tc.command+"/"+tc.option, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			cmd := newRootCommand()
			cmd.SetOut(&stdout)
			cmd.SetErr(&stderr)
			cmd.SetArgs([]string{"__complete", tc.command, tc.option, ""})
			if err := cmd.Execute(); err != nil {
				t.Fatalf("complete: %v; stderr=%q", err, &stderr)
			}
			if got, want := strings.TrimSpace(stdout.String()), fmt.Sprintf(":%d", tc.directive); got != want {
				t.Fatalf("completion = %q, want %q; stderr=%q", got, want, &stderr)
			}
		})
	}
}
