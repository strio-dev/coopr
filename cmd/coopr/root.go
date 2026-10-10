package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

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
	root.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error {
		return fmt.Errorf("%w\nSee '%s --help'", err, cmd.CommandPath())
	})
	root.AddCommand(newBuildCommandWithGlobals(false))
	root.AddCommand(newCopyCommand(oci.Image))
	root.AddCommand(newImageCommand())
	root.AddCommand(newImagesCommand())
	root.AddCommand(newComponentCommandWithGlobals(false))
	root.AddCommand(newComponentListCommand("components"))
	root.AddCommand(newSystemCommand())
	root.AddCommand(newLoginCommand(), newLogoutCommand())
	root.AddCommand(newManifestCommand(), newInfoCommand())
	remove := newImageRemoveCommand()
	remove.Use = "rmi [image...]"
	root.AddCommand(remove, newImageInspectCommand(), newCompletionCommand())
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
	err := root.ExecuteContext(ctx)
	if err != nil {
		if message := err.Error(); message != "" {
			if !strings.HasPrefix(message, "Error: ") {
				message = "Error: " + message
			}
			_, _ = fmt.Fprintln(stderr, message)
		}
		var exit interface{ ExitCode() int }
		if errors.As(err, &exit) {
			return exit.ExitCode()
		}
		return 125
	}
	return 0
}
