package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

func TestCommandHelpUsesOptionsAndHidesInternalOptions(t *testing.T) {
	root := newRootCommand()
	root.InitDefaultHelpCmd()
	root.InitDefaultCompletionCmd()
	var visit func(*cobra.Command)
	visit = func(command *cobra.Command) {
		args := strings.Fields(strings.TrimPrefix(command.CommandPath(), "coopr"))
		t.Run(command.CommandPath(), func(t *testing.T) {
			var out, errOut bytes.Buffer
			if status := run(append(args, "--help"), &out, &errOut); status != 0 || errOut.Len() != 0 {
				t.Fatalf("help status=%d stderr=%q", status, errOut.String())
			}
			help := out.String()
			if !strings.Contains(help, "Options:\n") || (!command.DisableFlagsInUseLine && !strings.Contains(help, "[options]")) {
				t.Fatalf("help lacks option terminology:\n%s", help)
			}
			if strings.Contains(help, "Flags:") || strings.Contains(help, "[flags]") {
				t.Fatalf("help retains flag terminology:\n%s", help)
			}
			command.LocalFlags().VisitAll(func(flag *pflag.Flag) {
				if flag.Hidden && strings.Contains(help, "--"+flag.Name) {
					t.Errorf("hidden option --%s appears in help", flag.Name)
				}
			})
			if command.HasParent() && !strings.Contains(help, "Global Options:\n") {
				t.Fatalf("help omits inherited option group:\n%s", help)
			}
		})
		for _, child := range command.Commands() {
			if !child.Hidden {
				visit(child)
			}
		}
	}
	visit(root)
}

func TestHelpSubcommandAndBareGroups(t *testing.T) {
	for _, args := range [][]string{{"help", "component", "build"}, {"image"}, {"component"}, {"manifest"}, {"system"}} {
		var out, errOut bytes.Buffer
		if status := run(args, &out, &errOut); status != 0 || errOut.Len() != 0 {
			t.Fatalf("%v status=%d stderr=%q", args, status, errOut.String())
		}
		if !strings.Contains(out.String(), "Options:\n") || strings.Contains(out.String(), "Flags:") {
			t.Fatalf("%v help terminology:\n%s", args, out.String())
		}
	}
}

func TestHiddenOptionsStayHiddenInHelpAndCompletion(t *testing.T) {
	newCommand := func() *cobra.Command {
		root := newRootCommandWithStorageNamespace(func() error {
			t.Fatal("help or completion prepared native storage")
			return nil
		})
		root.PersistentFlags().String("private-global", "", "internal global option")
		root.PersistentFlags().String("shadowed-option", "", "public global option")
		if err := root.PersistentFlags().MarkHidden("private-global"); err != nil {
			t.Fatal(err)
		}
		probe := &cobra.Command{Use: "probe <input>", Short: "Probe help and completion", Run: func(*cobra.Command, []string) {}}
		probe.Flags().String("private-local", "", "internal local option")
		probe.Flags().String("public-local", "", "public local option")
		probe.Flags().String("shadowed-option", "", "internal local override")
		for _, name := range []string{"private-local", "shadowed-option"} {
			if err := probe.Flags().MarkHidden(name); err != nil {
				t.Fatal(err)
			}
		}
		root.AddCommand(probe)
		return root
	}
	for _, args := range [][]string{{"probe", "--help"}, {"__complete", "probe", "--"}} {
		root := newCommand()
		var out, errOut bytes.Buffer
		root.SetOut(&out)
		root.SetErr(&errOut)
		root.SetArgs(args)
		if err := root.ExecuteContext(context.Background()); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		for _, name := range []string{"private-global", "private-local", "shadowed-option"} {
			if strings.Contains(out.String(), "--"+name) {
				t.Errorf("%v exposes hidden --%s:\n%s", args, name, out.String())
			}
		}
		for _, name := range []string{"public-local", "log-level"} {
			if !strings.Contains(out.String(), "--"+name) {
				t.Errorf("%v omits visible --%s:\n%s", args, name, out.String())
			}
		}
	}
}

func TestHiddenBuildOptionsAreNotCompleted(t *testing.T) {
	for _, path := range [][]string{{"build"}, {"component", "build"}} {
		var out, errOut bytes.Buffer
		args := append([]string{"__complete"}, path...)
		if status := run(append(args, "--"), &out, &errOut); status != 0 {
			t.Fatalf("%v completion status=%d stderr=%q", path, status, errOut.String())
		}
		for _, name := range []string{"blob-cache", "log-rusage", "rusage-logfile", "pull-always", "pull-never", "cdi-config-dir", "signature-policy"} {
			if strings.Contains(out.String(), "--"+name) {
				t.Errorf("%v completes hidden --%s", path, name)
			}
		}
		if !strings.Contains(out.String(), "--file") || !strings.Contains(out.String(), "--log-level") {
			t.Fatalf("%v completion omits visible options:\n%s", path, out.String())
		}
	}
}

func TestCompletionShellsAndInvalidShells(t *testing.T) {
	for _, shell := range []string{"bash", "zsh", "fish", "powershell"} {
		var out, errOut bytes.Buffer
		if status := run([]string{"completion", shell}, &out, &errOut); status != 0 || !strings.Contains(out.String(), "coopr") || errOut.Len() != 0 {
			t.Fatalf("%s completion status=%d stdout=%q stderr=%q", shell, status, out.String(), errOut.String())
		}
	}
	for _, args := range [][]string{{"completion", "unknown"}, {"completion", "bash", "extra"}, {"--unknown-option"}} {
		var out, errOut bytes.Buffer
		if status := run(args, &out, &errOut); status == 0 || out.Len() != 0 || errOut.Len() == 0 {
			t.Fatalf("%v status=%d stdout=%q stderr=%q", args, status, out.String(), errOut.String())
		}
	}
}
