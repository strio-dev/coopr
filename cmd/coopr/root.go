package main

import (
	"context"
	"fmt"
	"io"

	"coopr/internal/oci"
	"github.com/spf13/cobra"
)

var version = "dev"

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
	root.AddCommand(newBuildCommandWithGlobals(false))
	root.AddCommand(newCopyCommandWithGlobals(oci.Image, false))
	root.AddCommand(newImageCommand())
	root.AddCommand(newImagesCommand())
	root.AddCommand(newComponentCommandWithGlobals(false))
	root.AddCommand(newSystemCommand())
	root.AddCommand(newCacheCommand())
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
	if err := root.ExecuteContext(ctx); err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}
