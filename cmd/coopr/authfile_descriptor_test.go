package main

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/strio-dev/coopr/internal/buildah"
	"github.com/strio-dev/coopr/internal/oci"
)

func TestRegistryCommandMaterializesDescriptorAuthFile(t *testing.T) {
	for _, outcome := range []struct {
		name string
		err  error
	}{
		{name: "success"},
		{name: "failure", err: errors.New("operation failed")},
		{name: "incomplete", err: buildah.ErrWorkerWaitIncomplete},
	} {
		for _, fromEnvironment := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/environment=%t", outcome.name, fromEnvironment), func(t *testing.T) {
				r, w, err := os.Pipe()
				if err != nil {
					t.Fatal(err)
				}
				defer func() {
					if err := r.Close(); err != nil {
						t.Error(err)
					}
				}()
				const data = `{"auths":{}}`
				if _, err := w.Write([]byte(data)); err != nil {
					t.Fatal(err)
				}
				if err := w.Close(); err != nil {
					t.Fatal(err)
				}
				descriptor := fmt.Sprintf("/dev/fd/%d", r.Fd())
				t.Setenv("REGISTRY_AUTH_FILE", "")
				if fromEnvironment {
					t.Setenv("REGISTRY_AUTH_FILE", descriptor)
				}
				var flags registryFlags
				var owned string
				cmd := &cobra.Command{Use: "probe", SilenceErrors: true, SilenceUsage: true, RunE: func(cmd *cobra.Command, _ []string) error {
					path, _, err := oci.NormalizeRegistryPaths(flags.authFileOption(cmd), "")
					if err != nil {
						return err
					}
					if path == descriptor || strings.HasPrefix(path, "/dev/fd/") {
						return errors.New("descriptor was not materialized before execution")
					}
					owned = path
					for range 2 {
						contents, err := os.ReadFile(path)
						if err != nil || string(contents) != data {
							t.Fatalf("reusable authfile = %q, %v", contents, err)
						}
					}
					info, err := os.Stat(path)
					if err != nil || info.Mode().Perm() != 0o600 {
						t.Fatalf("private authfile permissions: %v, %v", info, err)
					}
					return outcome.err
				}}
				flags.addTo(cmd)
				if !fromEnvironment {
					cmd.SetArgs([]string{"--authfile", descriptor})
				} else {
					cmd.SetArgs(nil)
				}
				err = cmd.Execute()
				if !errors.Is(err, outcome.err) || owned == "" {
					t.Fatalf("execution error = %v, owned authfile = %q", err, owned)
				}
				t.Cleanup(func() { _ = os.Remove(owned) })
				_, statErr := os.Stat(owned)
				if errors.Is(outcome.err, buildah.ErrWorkerWaitIncomplete) {
					if statErr != nil || !strings.Contains(err.Error(), owned) {
						t.Fatalf("incomplete worker lost private authfile: %v, %v", err, statErr)
					}
				} else if !errors.Is(statErr, os.ErrNotExist) {
					t.Fatalf("completed operation retained authfile: %v", statErr)
				}
			})
		}
	}
}
