package main

import (
	"github.com/spf13/cobra"
	"go.podman.io/buildah/define"
	"go.podman.io/buildah/pkg/parse"
)

var sbomFlagNames = []string{"sbom", "sbom-scanner-image", "sbom-scanner-command", "sbom-merge-strategy", "sbom-output", "sbom-image-output", "sbom-purl-output", "sbom-image-purl-output"}

func addSBOMFlags(cmd *cobra.Command) {
	f := cmd.Flags()
	f.String("sbom", "", "SBOM scanner preset: syft, syft-cyclonedx, syft-spdx, trivy, trivy-cyclonedx, or trivy-spdx")
	f.Lookup("sbom").NoOptDefVal = "syft"
	f.String("sbom-scanner-image", "", "image containing a custom SBOM scanner")
	f.StringArray("sbom-scanner-command", nil, "scanner command using {ROOTFS}, {CONTEXT}, and {OUTPUT} (repeatable)")
	f.String("sbom-merge-strategy", "", "merge scanner results: cat, merge-cyclonedx-by-component-name-and-version, or merge-spdx-by-package-name-and-versioninfo")
	f.String("sbom-output", "", "write SBOM to a host file")
	f.String("sbom-image-output", "", "write SBOM to a path in the image")
	f.String("sbom-purl-output", "", "write package URLs to a host file")
	f.String("sbom-image-purl-output", "", "write package URLs to a path in the image")
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
