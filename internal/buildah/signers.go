package buildah

import (
	"fmt"
	"io"
	"os"

	sigstorecli "go.podman.io/image/v5/pkg/cli/sigstore"
	"go.podman.io/image/v5/signature/signer"
	"go.podman.io/image/v5/signature/simplesequoia"
)

// NativeSigners prepares upstream signers in the process that performs the copy.
// Parameter files carry their own passphrase settings; no key material crosses
// the supervised worker protocol.
func NativeSigners(parameterFile, sequoiaFingerprint string, sigstorePassphrase []byte, gpgPassphrase string, interactionOutput io.Writer) ([]*signer.Signer, func(), error) {
	var signers []*signer.Signer
	cleanup := func() {
		for _, signer := range signers {
			_ = signer.Close() // Cleanup has no error channel, matching upstream signer cleanup.
		}
	}
	if parameterFile != "" {
		native, err := sigstorecli.NewSignerFromParameterFile(parameterFile, &sigstorecli.Options{
			PrivateKeyPassphrasePrompt: func(string) (string, error) {
				if len(sigstorePassphrase) != 0 {
					return string(sigstorePassphrase), nil
				}
				return gpgPassphrase, nil
			},
			Stdin: os.Stdin, Stdout: interactionOutput,
		})
		if err != nil {
			cleanup()
			return nil, func() {}, err
		}
		signers = append(signers, native)
	}
	if sequoiaFingerprint != "" {
		native, err := simplesequoia.NewSigner(simplesequoia.WithKeyFingerprint(sequoiaFingerprint), simplesequoia.WithPassphrase(gpgPassphrase))
		if err != nil {
			cleanup()
			return nil, func() {}, fmt.Errorf("using --sign-by-sq-fingerprint: %w", err)
		}
		signers = append(signers, native)
	}
	return signers, cleanup, nil
}
