package main

import (
	"context"
	"fmt"
	"io"

	"coopr/internal/oci"
	"github.com/spf13/cobra"
)

func newRootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:           "coopr",
		Short:         "Compose OCI container builds and reusable components",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(newBuildCommand())
	root.AddCommand(newCopyCommand(oci.Image))
	root.AddCommand(newImageCommand())
	root.AddCommand(newImagesCommand())
	root.AddCommand(newComponentCommand())
	root.AddCommand(newComponentsCommand())
	root.AddCommand(newSystemCommand())
	root.AddCommand(newCacheCommand())
	addGlobalFlags(root)
	return root
}

func run(args []string, stdout, stderr io.Writer) int {
	return runContext(context.Background(), args, stdout, stderr)
}

func runContext(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	root := newRootCommand()
	root.SetArgs(args)
	root.SetOut(stdout)
	root.SetErr(stderr)
	if err := root.ExecuteContext(ctx); err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}
