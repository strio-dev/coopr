package main

import (
	"context"
	"errors"
	"fmt"
	"io"

	"coopr/internal/oci"

	"github.com/spf13/cobra"
)

var version = "dev"

type commandExitError struct {
	Code int
	Err  error
}

func (err commandExitError) Error() string {
	if err.Err == nil {
		return ""
	}
	return err.Err.Error()
}

func (err commandExitError) Unwrap() error { return err.Err }
func (err commandExitError) ExitCode() int { return err.Code }

func newRootCommand() *cobra.Command {
	return newRootCommandWithStorageNamespace(nil)
}

func newRootCommandWithStorageNamespace(prepareNamespace func() error) *cobra.Command {
	root := &cobra.Command{
		Use:           "coopr",
		Version:       version,
		Short:         "Compose OCI container builds and reusable components",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.SetUsageTemplate(usageTemplate)
	root.AddCommand(newBuildCommandWithGlobals(false))
	root.AddCommand(newCopyCommand(oci.Image))
	root.AddCommand(newImageCommand())
	root.AddCommand(newImagesCommand())
	root.AddCommand(newComponentCommandWithGlobals(false))
	root.AddCommand(newComponentListCommand("components"))
	root.AddCommand(newSystemCommand())
	root.AddCommand(newLoginCommand(), newLogoutCommand())
	root.AddCommand(newManifestCommand(), newInfoCommand())
	root.AddCommand(newImagePullCommand(), newImagePushCommand(), newImageTagCommand(), newImageSaveCommand(), newImageLoadCommand(), newImageExistsCommand(), newImageHistoryCommand())
	addGlobalFlags(root, prepareNamespace)
	return root
}

func run(args []string, stdout, stderr io.Writer) int {
	return runContext(context.Background(), args, stdout, stderr)
}

func runContext(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return runContextWithStorageNamespace(ctx, args, stdout, stderr, nil)
}

func runContextWithStorageNamespace(ctx context.Context, args []string, stdout, stderr io.Writer, prepareNamespace func() error) int {
	root := newRootCommandWithStorageNamespace(prepareNamespace)
	root.SetArgs(args)
	root.SetOut(stdout)
	root.SetErr(stderr)
	root.InitDefaultCompletionCmd()
	for _, command := range root.Commands() {
		if command.Name() == "completion" {
			// Make the shell group validate Cobra's NoArgs contract before showing help.
			command.RunE = helpOnNoArgs
			break
		}
	}
	command, err := root.ExecuteContextC(ctx)
	if err != nil {
		if err.Error() != "" {
			_, _ = fmt.Fprintln(stderr, err)
		}
		var exit interface{ ExitCode() int }
		if errors.As(err, &exit) {
			return exit.ExitCode()
		}
		if command != nil && command.Name() == "exists" {
			return 125
		}
		return 1
	}
	return 0
}
