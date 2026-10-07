package main

import (
	"fmt"
	"text/template"

	"coopr/internal/buildah"

	"github.com/spf13/cobra"
	upstream "go.podman.io/buildah"
	"go.podman.io/buildah/define"
	"go.podman.io/storage"
)

func newInfoCommand() *cobra.Command {
	var format string
	cmd := imageIOCommand("info", "Show native storage and host information", cobra.NoArgs, func(cmd *cobra.Command, _ []string) error {
		var tmpl *template.Template
		var err error
		if format != "" && format != "json" {
			tmpl, err = template.New("info").Parse(format)
			if err != nil {
				return err
			}
		}
		options, err := commandStorage(cmd)
		if err != nil {
			return err
		}
		info := map[string]any{"version": map[string]string{"coopr": version, "buildah": define.Version}}
		err = buildah.WithImageStore(cmd.Context(), options, func(backend storage.Store) error {
			// Check status explicitly: native Info logs storage errors while returning
			// partial results, but a CLI operation must report an operational failure.
			if _, err := backend.Status(); err != nil {
				return err
			}
			data, err := upstream.Info(backend)
			if err != nil {
				return err
			}
			for _, entry := range data {
				info[entry.Type] = entry.Data
			}
			return nil
		})
		if err != nil {
			return err
		}
		if tmpl != nil {
			if err := tmpl.Execute(cmd.OutOrStdout(), info); err != nil {
				return err
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout())
			return err
		}
		return writeJSON(cmd, info)
	})
	cmd.Flags().StringVar(&format, "format", "json", "output format: json or a Go template")
	return cmd
}
