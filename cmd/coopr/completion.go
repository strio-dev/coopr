package main

import (
	"errors"
	"os"

	"github.com/spf13/cobra"
)

func newCompletionCommand() *cobra.Command {
	var filename string
	var noDescriptions bool
	cmd := &cobra.Command{
		Use:       "completion {bash|zsh|fish|powershell}",
		Short:     "Generate shell completions",
		ValidArgs: []string{"bash", "zsh", "fish", "powershell"},
		Args:      cobra.MatchAll(cobra.ExactArgs(1), cobra.OnlyValidArgs),
		RunE: func(cmd *cobra.Command, args []string) (retErr error) {
			out := cmd.OutOrStdout()
			if filename != "" {
				file, err := os.Create(filename)
				if err != nil {
					return err
				}
				defer func() { retErr = errors.Join(retErr, file.Close()) }()
				out = file
			}
			switch args[0] {
			case "bash":
				return cmd.Root().GenBashCompletionV2(out, !noDescriptions)
			case "zsh":
				if noDescriptions {
					return cmd.Root().GenZshCompletionNoDesc(out)
				}
				return cmd.Root().GenZshCompletion(out)
			case "fish":
				return cmd.Root().GenFishCompletion(out, !noDescriptions)
			default:
				if noDescriptions {
					return cmd.Root().GenPowerShellCompletion(out)
				}
				return cmd.Root().GenPowerShellCompletionWithDesc(out)
			}
		},
	}
	cmd.Flags().StringVarP(&filename, "file", "f", "", "write completion to a file")
	cmd.Flags().BoolVar(&noDescriptions, "no-desc", false, "omit completion descriptions")
	return cmd
}
