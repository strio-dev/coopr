package main

import (
	"github.com/spf13/cobra"
	"github.com/strio-dev/coopr/internal/transfer"
)

type signingFlags struct {
	signBy             string
	parameterFile      string
	sequoiaFingerprint string
	privateKeyFile     string
	passphraseFile     string
}

func (flags *signingFlags) addTo(command *cobra.Command) {
	command.Flags().StringVar(&flags.parameterFile, "sign-by-sigstore", "", "sign using a Sigstore parameter file")
	command.Flags().StringVar(&flags.sequoiaFingerprint, "sign-by-sq-fingerprint", "", "sign using a Sequoia-PGP key fingerprint (automatically uses optional coopr-sequoia-signing helper)")
	command.Flags().StringVar(&flags.signBy, "sign-by", "", "sign a local or registry image with the GPG key fingerprint")
	command.Flags().StringVar(&flags.privateKeyFile, "sign-by-sigstore-private-key", "", "sign a registry image with the Sigstore private key at path")
	command.Flags().StringVar(&flags.passphraseFile, "sign-passphrase-file", "", "read the signing-key passphrase from path (Sigstore defaults to COSIGN_PASSWORD)")
}

func (flags signingFlags) options() transfer.SigningOptions {
	return transfer.SigningOptions{SignBy: flags.signBy, SigstoreParameterFile: flags.parameterFile, SequoiaFingerprint: flags.sequoiaFingerprint, SigstorePrivateKeyFile: flags.privateKeyFile, PassphraseFile: flags.passphraseFile}
}
