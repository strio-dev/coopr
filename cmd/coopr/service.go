package main

import (
	"context"
	"errors"
	"io"
	"os"
	"syscall"

	"github.com/spf13/cobra"
	"github.com/strio-dev/coopr/internal/apibackend"
	"github.com/strio-dev/coopr/internal/apiservice"
)

func newSystemServiceCommand() *cobra.Command {
	var socket string
	var parentFD int
	cmd := &cobra.Command{Use: "service", Short: "Serve the local execution API on a private Unix socket", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if socket == "" {
			return errors.New("--socket is required")
		}
		ctx, cancel := context.WithCancel(cmd.Context())
		defer cancel()
		if parentFD >= 0 {
			if parentFD < 3 {
				return errors.New("--parent-fd must identify an inherited descriptor >= 3")
			}
			parent := os.NewFile(uintptr(parentFD), "coopr-parent")
			if parent == nil {
				return errors.New("invalid parent descriptor")
			}
			info, err := parent.Stat()
			if err != nil {
				_ = parent.Close()
				return err
			}
			if info.Mode()&os.ModeNamedPipe == 0 {
				_ = parent.Close()
				return errors.New("--parent-fd must identify a pipe")
			}
			syscall.CloseOnExec(parentFD)
			defer func() { _ = parent.Close() }()
			go func() { _, _ = io.Copy(io.Discard, parent); cancel() }()
		}
		return apiservice.Serve(ctx, socket, version, apibackend.Execute)
	}}
	cmd.Flags().StringVar(&socket, "socket", "", "absolute Unix socket path inside a private directory")
	cmd.Flags().IntVar(&parentFD, "parent-fd", -1, "inherited parent-lifetime pipe; EOF shuts down the service")
	return cmd
}
