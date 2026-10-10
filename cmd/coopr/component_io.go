package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"coopr/internal/componentstore"
	"coopr/internal/localstore"
	"coopr/internal/oci"
	"coopr/internal/storeactivity"
	"coopr/internal/transfer"

	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/spf13/cobra"
)

func newComponentPullCommand() *cobra.Command {
	var registry registryFlags
	var tag string
	cmd := &cobra.Command{Use: "pull <reference>", Short: "Pull a component and all its platforms into the local store", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		resolver, err := oci.NewResolver(registry.resolverOptions(cmd))
		if err != nil {
			return err
		}
		root, err := resolver.PullComponent(cmd.Context(), strings.TrimPrefix(args[0], "registry:"), tag)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(cmd.OutOrStdout(), transfer.LocalReference(tag, root))
		return err
	}}
	registry.addTo(cmd)
	cmd.Flags().StringVarP(&tag, "tag", "t", "", "assign a local component name; otherwise print its digest")
	return cmd
}

func newComponentPushCommand() *cobra.Command {
	var registry registryFlags
	cmd := &cobra.Command{Use: "push <source> <destination>", Short: "Push a local component to a registry", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
		destination, err := transfer.ParsePushDestination(args[1], oci.Component)
		if err != nil {
			return err
		}
		result, err := transfer.Copy(cmd.Context(), oci.Component, args[0], destination, registry.transferOptions(cmd))
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(cmd.OutOrStdout(), result)
		return err
	}}
	registry.addTo(cmd)
	return cmd
}

func newComponentTagCommand() *cobra.Command {
	return &cobra.Command{Use: "tag <source> <name>...", Short: "Assign local component names without rebuilding", Args: cobra.MinimumNArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
		destinations := make([]transfer.Destination, len(args)-1)
		for i, name := range args[1:] {
			var err error
			destinations[i], err = transfer.ParseDestination("local:"+name, oci.Component)
			if err != nil {
				return err
			}
		}
		for _, destination := range destinations {
			if _, err := transfer.Copy(cmd.Context(), oci.Component, args[0], destination, transfer.Options{}); err != nil {
				return err
			}
		}
		return nil
	}}
}

func newComponentSaveCommand() *cobra.Command {
	var output string
	cmd := &cobra.Command{Use: "save <name-or-digest>", Short: "Save a component and all its platforms as an OCI archive", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) (retErr error) {
		if output != "" && output != "-" {
			destination, err := transfer.ParseDestination("oci-archive:"+output, oci.Component)
			if err != nil {
				return err
			}
			_, err = transfer.Copy(cmd.Context(), oci.Component, args[0], destination, transfer.Options{})
			return err
		}
		dir, err := componentstore.DefaultDir()
		if err != nil {
			return err
		}
		activity, err := storeactivity.AcquireShared(cmd.Context(), dir)
		if err != nil {
			return err
		}
		defer func() { retErr = errors.Join(retErr, activity.Close()) }()
		store, err := componentstore.Open(cmd.Context(), dir)
		if err != nil {
			return err
		}
		root, err := store.Resolve(cmd.Context(), strings.TrimPrefix(args[0], "local:"))
		if err != nil {
			return err
		}
		return localstore.WriteArchiveTo(cmd.Context(), store, root, cmd.OutOrStdout())
	}}
	cmd.Flags().StringVarP(&output, "output", "o", "", "write the OCI archive to a file (default: stdout)")
	return cmd
}

func newComponentLoadCommand() *cobra.Command {
	var input, tag string
	cmd := &cobra.Command{Use: "load", Short: "Load a component from an OCI archive", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		path := input
		if path == "" || path == "-" {
			file, err := os.CreateTemp("", "coopr-component-load-*.tar")
			if err != nil {
				return err
			}
			path = file.Name()
			defer func() { _ = os.Remove(path) }()
			_, copyErr := io.Copy(file, cmd.InOrStdin())
			if err := errors.Join(copyErr, file.Close()); err != nil {
				return err
			}
		}
		resolver, err := oci.NewResolver(oci.Options{})
		if err != nil {
			return err
		}
		root, err := resolver.LoadComponent(cmd.Context(), path, tag)
		if err != nil {
			return err
		}
		if tag == "" {
			tag = root.Annotations[v1.AnnotationRefName]
		}
		_, err = fmt.Fprintln(cmd.OutOrStdout(), transfer.LocalReference(tag, root))
		return err
	}}
	cmd.Flags().StringVarP(&input, "input", "i", "", "read an OCI archive from a file (default: stdin)")
	cmd.Flags().StringVarP(&tag, "tag", "t", "", "assign a local component name (default: archived name, or digest if unnamed)")
	return cmd
}

func newComponentExistsCommand() *cobra.Command {
	return &cobra.Command{Use: "exists <name-or-digest>", Short: "Check whether a component exists locally", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		dir, err := componentstore.DefaultDir()
		if err != nil {
			return commandExitError{Code: 125, Err: err}
		}
		activity, err := storeactivity.AcquireShared(cmd.Context(), dir)
		if err != nil {
			return commandExitError{Code: 125, Err: err}
		}
		_, found, err := localstore.Inspect(cmd.Context(), dir, strings.TrimPrefix(args[0], "local:"))
		if err := errors.Join(err, activity.Close()); err != nil {
			return commandExitError{Code: 125, Err: err}
		}
		if !found {
			return commandExitError{Code: 1}
		}
		return nil
	}}
}
