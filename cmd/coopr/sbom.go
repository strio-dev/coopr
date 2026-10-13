package main

import (
	"github.com/spf13/cobra"
	"go.podman.io/buildah/define"
	buildahcli "go.podman.io/buildah/pkg/cli"
	"go.podman.io/buildah/pkg/parse"
)

var sbomFlagNames = []string{"sbom", "sbom-scanner-image", "sbom-scanner-command", "sbom-merge-strategy", "sbom-output", "sbom-image-output", "sbom-purl-output", "sbom-image-purl-output"}

func addSBOMFlags(cmd *cobra.Command) {
	native := buildahcli.GetBudFlags(new(buildahcli.BudResults))
	native.Lookup("sbom").NoOptDefVal = "syft"
	addNativeFlags(cmd, &native, buildahcli.GetBudFlagsCompletions(), sbomFlagNames...)
}

func parseSBOMFlags(cmd *cobra.Command) ([]define.SBOMScanOptions, error) {
	for _, name := range sbomFlagNames {
		if cmd.Flags().Changed(name) {
			options, err := parse.SBOMScanOptions(cmd)
			if err != nil {
				return nil, err
			}
			return []define.SBOMScanOptions{*options}, nil
		}
	}
	return nil, nil
}
