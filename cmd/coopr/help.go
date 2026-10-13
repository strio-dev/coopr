package main

import (
	"strings"

	"github.com/spf13/cobra"
)

func init() {
	// Cobra's UseLine preserves argument syntax and aliases, but its option label is fixed.
	cobra.AddTemplateFunc("optionsUseLine", func(command *cobra.Command) string {
		return strings.Replace(command.UseLine(), "[flags]", "[options]", 1)
	})
}

// Customize terminology while leaving visibility, grouping and traversal to Cobra.
func optionsUsageTemplate(command *cobra.Command) string {
	return strings.NewReplacer(
		"{{.UseLine}}", "{{optionsUseLine .}}",
		"\n  {{.CommandPath}} [command]", "\n  {{.CommandPath}}{{if .HasAvailableFlags}} [options]{{end}} [command]",
		"Global Flags:", "Global Options:",
		"Flags:", "Options:",
	).Replace(command.UsageTemplate())
}
