package main

import (
	"coopr/internal/oci"

	"github.com/spf13/cobra"
	"go.podman.io/common/pkg/auth"
	"go.podman.io/common/pkg/completion"
	"go.podman.io/image/v5/types"
)

func newLoginCommand() *cobra.Command {
	options := auth.LoginOptions{AcceptUnspecifiedRegistry: true}
	var tlsVerify bool
	cmd := &cobra.Command{Use: "login [registry]", Short: "Log in to a container registry", Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		options.Stdin, options.Stdout = cmd.InOrStdin(), cmd.OutOrStdout()
		system := &types.SystemContext{}
		if cmd.Flags().Changed("tls-verify") {
			oci.ApplyTLSVerify(system, &tlsVerify)
		}
		return auth.Login(cmd.Context(), system, &options, args)
	}}
	cmd.Flags().AddFlagSet(auth.GetLoginFlags(&options))
	completion.CompleteCommandFlags(cmd, auth.GetLoginFlagsCompletions())
	cmd.Flags().BoolVar(&tlsVerify, "tls-verify", true, "verify registry TLS certificates (default: registry configuration)")
	return cmd
}

func newLogoutCommand() *cobra.Command {
	options := auth.LogoutOptions{AcceptUnspecifiedRegistry: true}
	cmd := &cobra.Command{Use: "logout [registry]", Short: "Log out of a container registry", Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		options.Stdout = cmd.OutOrStdout()
		return auth.Logout(nil, &options, args)
	}}
	cmd.Flags().AddFlagSet(auth.GetLogoutFlags(&options))
	completion.CompleteCommandFlags(cmd, auth.GetLogoutFlagsCompletions())
	return cmd
}
