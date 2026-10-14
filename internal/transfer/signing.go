package transfer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	imagecli "go.podman.io/image/v5/pkg/cli"
	"os"
	"path/filepath"
	"strings"

	"coopr/internal/buildah"
	"coopr/internal/imagestore"
	"coopr/internal/oci"
	"github.com/distribution/reference"
	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"go.podman.io/common/pkg/retry"
	imagecopy "go.podman.io/image/v5/copy"
	"go.podman.io/image/v5/docker"
	"go.podman.io/image/v5/pkg/cli/sigstore/params"
	"go.podman.io/image/v5/signature"
	"go.podman.io/image/v5/types"
)

// SigningOptions configures image signing during native image publication.
// The passphrase is loaded only when the transfer starts, so secret bytes never
// enter a build request, worker protocol, cache key, or log message.
type SigningOptions struct {
	SignBy                 string
	SigstoreParameterFile  string
	SequoiaFingerprint     string
	SigstorePrivateKeyFile string
	PassphraseFile         string
	sigstorePassword       *[]byte
}

func (options SigningOptions) Enabled() bool {
	return options.SignBy != "" || options.SigstorePrivateKeyFile != "" || options.SigstoreParameterFile != "" || options.SequoiaFingerprint != ""
}

// ContextArtifacts identifies signing credentials that must be excluded from
// build inputs. GPGME uses the ambient GnuPG home rather than a key-file option.
func (options SigningOptions) ContextArtifacts() ([]string, error) {
	paths := []string{options.SigstorePrivateKeyFile, options.PassphraseFile}
	if options.SigstoreParameterFile != "" {
		paths = append(paths, options.SigstoreParameterFile)
		parameters, err := params.ParseFile(options.SigstoreParameterFile)
		if err != nil {
			return nil, err
		}
		paths = append(paths, parameters.PrivateKeyFile, parameters.PrivateKeyPassphraseFile)
	}
	if options.SequoiaFingerprint != "" {
		home := os.Getenv("SEQUOIA_HOME")
		if home == "" {
			config := os.Getenv("XDG_DATA_HOME")
			if config == "" {
				userHome, err := os.UserHomeDir()
				if err != nil {
					return nil, err
				}
				config = filepath.Join(userHome, ".local", "share")
			}
			home = filepath.Join(config, "sequoia")
		}
		paths = append(paths, home)
	}
	if options.SignBy != "" {
		home := os.Getenv("GNUPGHOME")
		if home == "" {
			userHome, err := os.UserHomeDir()
			if err != nil {
				return nil, fmt.Errorf("resolve GnuPG home: %w", err)
			}
			home = filepath.Join(userHome, ".gnupg")
		}
		absolute, err := filepath.Abs(home)
		if err != nil {
			return nil, fmt.Errorf("resolve GnuPG home: %w", err)
		}
		paths = append(paths, absolute)
	}
	return paths, nil
}

// ValidateSigningDestination enforces image-signing mode and transport rules.
func ValidateSigningDestination(kind oci.Kind, destination Destination, options SigningOptions) error {
	if options.PassphraseFile != "" {
		count := 0
		for _, mode := range []string{options.SignBy, options.SequoiaFingerprint, options.SigstorePrivateKeyFile} {
			if mode != "" {
				count++
			}
		}
		if count > 1 {
			return errors.New("only one of --sign-by, --sign-by-sq-fingerprint and --sign-by-sigstore-private-key can be used with --sign-passphrase-file")
		}
	}
	if !options.Enabled() {
		if options.PassphraseFile != "" {
			return errors.New("--sign-passphrase-file requires --sign-by or --sign-by-sigstore-private-key, or another signing mode (--sign-by-sq-fingerprint or --sign-by-sigstore)")
		}
		return nil
	}
	if kind != oci.Image {
		return errors.New("signing is supported only for images")
	}
	if options.SigstorePrivateKeyFile != "" && destination.Transport == "local" {
		return errors.New("sigstore private-key signing requires a native push destination; Coopr local signing supports only --sign-by")
	}
	if (options.SigstoreParameterFile != "" || options.SequoiaFingerprint != "") && destination.Transport == "local" {
		return errors.New("parameter-file and Sequoia signing require a native push destination; Coopr local signing supports only --sign-by")
	}
	return nil
}

func publishImage(ctx context.Context, layout string, root v1.Descriptor, destination string, opts Options) (_ string, retErr error) {
	if _, err := oci.NewResolver(oci.Options{
		AuthFile: opts.AuthFile, CertDir: opts.CertDir, TLSVerify: opts.TLSVerify,
		Credentials: opts.Credentials, Retry: opts.Retry, RetrySet: opts.RetrySet, RetryDelay: opts.RetryDelay, DecryptionKeys: opts.DecryptionKeys, SignaturePolicyPath: opts.SignaturePolicyPath,
	}); err != nil {
		return "", err
	}
	src, err := imagestore.LayoutReference(layout, root)
	if err != nil {
		return "", err
	}
	return publishImageReference(ctx, src, root, destination, opts)
}

func publishImageReference(ctx context.Context, src types.ImageReference, root v1.Descriptor, destination string, opts Options) (string, error) {
	return publishImageReferenceSelection(ctx, src, root, destination, opts, imagecopy.CopyAllImages)
}

func publishImageReferenceSelection(ctx context.Context, src types.ImageReference, root v1.Descriptor, destination string, opts Options, selection imagecopy.ImageListSelection) (_ string, retErr error) {
	if needsSequoiaHelper(opts) {
		return transferWithSequoiaHelper(ctx, "publish-reference", src.Transport().Name()+":"+src.StringWithinTransport(), root, Destination{Transport: "registry", Name: destination}, opts, false, selection)
	}

	dst, err := docker.ParseReference("//" + destination)
	if err != nil {
		return "", fmt.Errorf("open registry destination %q: %w", destination, err)
	}
	authFile, certDir, err := oci.NormalizeRegistryPaths(opts.AuthFile, opts.CertDir)
	if err != nil {
		return "", err
	}
	sigstorePassphrase, gpgPassphrase, err := opts.Signing.passphrases()
	if err != nil {
		return "", err
	}
	if _, err := oci.ParseReference(destination); err != nil {
		return "", fmt.Errorf("parse registry destination: %w", err)
	}
	system, err := signedTransferSystemContext(authFile, certDir, "", opts)
	if err != nil {
		return "", err
	}
	if opts.Signing.SigstorePrivateKeyFile != "" || opts.Signing.SigstoreParameterFile != "" {
		registriesDir, err := buildah.SigstoreAttachmentsConfig(system, dst)
		if err != nil {
			return "", err
		}
		defer func() { retErr = errors.Join(retErr, os.RemoveAll(registriesDir)) }()
		system.RegistriesDirPath = registriesDir
	}
	policy, err := signature.DefaultPolicy(system)
	if err != nil {
		return "", fmt.Errorf("load image policy: %w", err)
	}
	policyContext, err := signature.NewPolicyContext(policy)
	if err != nil {
		return "", fmt.Errorf("initialize image policy: %w", err)
	}
	defer func() { retErr = errors.Join(retErr, policyContext.Destroy()) }()
	signers, cleanupSigners, err := buildah.NativeSigners(opts.Signing.SigstoreParameterFile, opts.Signing.SequoiaFingerprint, sigstorePassphrase, gpgPassphrase, opts.signingInteractionOutput())
	if err != nil {
		return "", err
	}
	defer cleanupSigners()
	copyOptions := &imagecopy.Options{
		Signers:      signers,
		ReportWriter: opts.ProgressWriter,
		SourceCtx:    system, DestinationCtx: system, PreserveDigests: true,
		ImageListSelection:               selection,
		SignBy:                           opts.Signing.SignBy,
		SignPassphrase:                   gpgPassphrase,
		SignBySigstorePrivateKeyFile:     opts.Signing.SigstorePrivateKeyFile,
		SignSigstorePrivateKeyPassphrase: sigstorePassphrase,
	}
	if err := opts.Push.Apply(copyOptions); err != nil {
		return "", err
	}
	var copiedDigest digest.Digest
	retryOptions, err := nativeRetryOptions(opts)
	if err != nil {
		return "", err
	}
	err = retry.IfNecessary(ctx, func() error {
		data, copyErr := imagecopy.Image(ctx, policyContext, dst, src, copyOptions)
		if copyErr == nil {
			copiedDigest = digest.FromBytes(data)
		}
		return copyErr
	}, retryOptions)
	if err != nil {
		return "", fmt.Errorf("publish image: %w", err)
	}
	named, err := reference.ParseNormalizedNamed(destination)
	if err != nil {
		return "", err
	}
	return reference.TrimNamed(named).String() + "@" + copiedDigest.String(), nil
}

func nativeRetryOptions(opts Options) (*retry.Options, error) {
	return oci.RegistryRetryOptions(oci.Options{
		Retry: opts.Retry, RetrySet: opts.RetrySet, RetryDelay: opts.RetryDelay,
	})
}

func publishStoredImage(ctx context.Context, imageID string, root v1.Descriptor, destination string, opts Options) (result string, retErr error) {
	transferOptions, cleanup, err := storedTransferOptions(imageID, root.Digest, opts)
	if err != nil {
		return "", err
	}
	defer func() { retErr = errors.Join(retErr, cleanup()) }()
	transferOptions.RegistryDestination = destination
	repository, err := buildah.TransferStoredImageSupervised(ctx, transferOptions)
	if err != nil {
		return "", fmt.Errorf("publish stored image: %w", err)
	}
	return repository, nil
}

func signedTransferSystemContext(authFile, certDir, registriesDir string, opts Options) (*types.SystemContext, error) {
	system := &types.SystemContext{
		AuthFilePath:         authFile,
		DockerCertPath:       certDir,
		DockerDaemonHost:     os.Getenv("DOCKER_HOST"),
		RegistriesDirPath:    registriesDir,
		BigFilesTemporaryDir: os.TempDir(),
		SignaturePolicyPath:  opts.SignaturePolicyPath,
	}
	oci.ApplyTLSVerify(system, opts.TLSVerify)
	if opts.Credentials != "" {
		username, password, _ := strings.Cut(opts.Credentials, ":")
		if username == "" {
			return nil, errors.New("registry credentials require a username")
		}
		system.DockerAuthConfig = &types.DockerAuthConfig{Username: username, Password: password}
	}
	return system, nil
}

func signStoredImage(ctx context.Context, imageID string, manifest digest.Digest, name string, opts Options) (retErr error) {
	transferOptions, cleanup, err := storedTransferOptions(imageID, manifest, opts)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, cleanup()) }()
	transferOptions.LocalName = name
	if _, err := buildah.TransferStoredImageSupervised(ctx, transferOptions); err != nil {
		return fmt.Errorf("sign local image %s: %w", name, err)
	}
	return nil
}

func storedTransferOptions(imageID string, manifest digest.Digest, opts Options) (buildah.StoredTransferOptions, func() error, error) {
	storeOptions, err := nativeStoreOptions(opts)
	if err != nil {
		return buildah.StoredTransferOptions{}, func() error { return nil }, err
	}
	sigstorePassphraseFile := ""
	cleanup := func() error { return nil }
	if (opts.Signing.SigstorePrivateKeyFile != "" || opts.Signing.SigstoreParameterFile != "") && opts.Signing.PassphraseFile == "" {
		if password := os.Getenv("COSIGN_PASSWORD"); password != "" {
			file, err := os.CreateTemp("", "coopr-signing-passphrase-*")
			if err != nil {
				return buildah.StoredTransferOptions{}, cleanup, fmt.Errorf("create signing passphrase file: %w", err)
			}
			sigstorePassphraseFile = file.Name()
			cleanup = func() error {
				if err := os.Remove(sigstorePassphraseFile); err != nil && !errors.Is(err, os.ErrNotExist) {
					return fmt.Errorf("remove signing passphrase file: %w", err)
				}
				return nil
			}
			if err := file.Chmod(0o600); err != nil {
				return buildah.StoredTransferOptions{}, func() error { return nil }, errors.Join(err, file.Close(), cleanup())
			}
			if _, err := file.WriteString(password); err != nil {
				return buildah.StoredTransferOptions{}, func() error { return nil }, errors.Join(err, file.Close(), cleanup())
			}
			if err := file.Close(); err != nil {
				return buildah.StoredTransferOptions{}, func() error { return nil }, errors.Join(err, cleanup())
			}
		}
	}
	return buildah.StoredTransferOptions{
		ProgressWriter:         opts.ProgressWriter,
		Push:                   opts.Push,
		Store:                  storeOptions,
		ImageID:                imageID,
		ExpectedManifest:       manifest,
		AuthFile:               opts.AuthFile,
		CertDir:                opts.CertDir,
		TLSVerify:              opts.TLSVerify,
		Credentials:            opts.Credentials,
		Retry:                  opts.Retry,
		RetrySet:               opts.RetrySet,
		RetryDelay:             opts.RetryDelay,
		SignaturePolicyPath:    opts.SignaturePolicyPath,
		SignBy:                 opts.Signing.SignBy,
		SigstoreParameterFile:  opts.Signing.SigstoreParameterFile,
		SequoiaFingerprint:     opts.Signing.SequoiaFingerprint,
		SigstorePrivateKeyFile: opts.Signing.SigstorePrivateKeyFile,
		SigningPassphraseFile:  opts.Signing.PassphraseFile,
		SigstorePassphraseFile: sigstorePassphraseFile,
	}, cleanup, nil
}

func (options SigningOptions) passphrases() ([]byte, string, error) {
	if options.PassphraseFile != "" {
		value, err := readPassphraseFile(options.PassphraseFile)
		if err != nil {
			return nil, "", err
		}
		if options.SignBy != "" || options.SequoiaFingerprint != "" {
			var sigstore []byte
			if options.sigstorePassword != nil {
				sigstore = *options.sigstorePassword
			}
			return sigstore, string(value), nil
		}
		return value, "", nil
	}
	if options.SigstorePrivateKeyFile != "" || options.SigstoreParameterFile != "" {
		if options.sigstorePassword != nil {
			return *options.sigstorePassword, "", nil
		}
		return []byte(os.Getenv("COSIGN_PASSWORD")), "", nil
	}
	return nil, "", nil
}

func readPassphraseFile(path string) ([]byte, error) {
	value, err := imagecli.ReadPassphraseFile(path)
	if err != nil {
		return nil, fmt.Errorf("read signing passphrase file: %w", err)
	}
	return []byte(value), nil
}

func sigstoreAttachmentsConfig() (string, error) {
	dir, err := os.MkdirTemp("", "coopr-registries.d-*")
	if err != nil {
		return "", fmt.Errorf("create signature registry configuration: %w", err)
	}
	config, err := json.Marshal(map[string]any{
		"default-docker": map[string]bool{"use-sigstore-attachments": true},
	})
	if err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, "coopr-signing.yaml"), config, 0600); err != nil {
		_ = os.RemoveAll(dir)
		return "", fmt.Errorf("write signature registry configuration: %w", err)
	}
	return dir, nil
}
