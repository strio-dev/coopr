package transfer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	"go.podman.io/image/v5/signature"
	"go.podman.io/image/v5/types"
)

// SigningOptions configures image signing during registry publication.
// The passphrase is loaded only when the transfer starts, so secret bytes never
// enter a build request, worker protocol, cache key, or log message.
type SigningOptions struct {
	SignBy                 string
	SigstorePrivateKeyFile string
	PassphraseFile         string
}

func (options SigningOptions) Enabled() bool {
	return options.SignBy != "" || options.SigstorePrivateKeyFile != ""
}

// ContextArtifacts identifies signing credentials that must be excluded from
// build inputs. GPGME uses the ambient GnuPG home rather than a key-file option.
func (options SigningOptions) ContextArtifacts() ([]string, error) {
	paths := []string{options.SigstorePrivateKeyFile, options.PassphraseFile}
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
	if options.SignBy != "" && options.SigstorePrivateKeyFile != "" {
		return errors.New("--sign-by and --sign-by-sigstore-private-key are mutually exclusive")
	}
	if !options.Enabled() {
		if options.PassphraseFile != "" {
			return errors.New("--sign-passphrase-file requires --sign-by or --sign-by-sigstore-private-key")
		}
		return nil
	}
	if kind != oci.Image {
		return errors.New("signing is supported only for images")
	}
	if options.SigstorePrivateKeyFile != "" && destination.Transport != "registry" {
		return fmt.Errorf("signing requires a registry destination; got %s transport", destination.Transport)
	}
	if options.SignBy != "" && destination.Transport != "registry" && destination.Transport != "local" {
		return fmt.Errorf("GPG signing requires a local or registry destination; got %s transport", destination.Transport)
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

func publishImageReference(ctx context.Context, src types.ImageReference, root v1.Descriptor, destination string, opts Options) (_ string, retErr error) {
	dst, err := docker.ParseReference("//" + destination)
	if err != nil {
		return "", fmt.Errorf("open registry destination %q: %w", destination, err)
	}
	authFile, certDir, err := oci.NormalizeRegistryPaths(opts.AuthFile, opts.CertDir)
	if err != nil {
		return "", err
	}
	registriesDir := ""
	if opts.Signing.SigstorePrivateKeyFile != "" {
		registriesDir, err = sigstoreAttachmentsConfig()
		if err != nil {
			return "", err
		}
		defer func() { retErr = errors.Join(retErr, os.RemoveAll(registriesDir)) }()
	}
	sigstorePassphrase, gpgPassphrase, err := opts.Signing.passphrases()
	if err != nil {
		return "", err
	}
	if _, err := oci.ParseReference(destination); err != nil {
		return "", fmt.Errorf("parse registry destination: %w", err)
	}
	system, err := signedTransferSystemContext(authFile, certDir, registriesDir, opts)
	if err != nil {
		return "", err
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
	copyOptions := &imagecopy.Options{
		SourceCtx: system, DestinationCtx: system, PreserveDigests: true,
		ImageListSelection:               imagecopy.CopyAllImages,
		SignBy:                           opts.Signing.SignBy,
		SignPassphrase:                   gpgPassphrase,
		SignBySigstorePrivateKeyFile:     opts.Signing.SigstorePrivateKeyFile,
		SignSigstorePrivateKeyPassphrase: sigstorePassphrase,
	}
	retryOptions, err := nativeRetryOptions(opts)
	if err != nil {
		return "", err
	}
	err = retry.IfNecessary(ctx, func() error {
		_, copyErr := imagecopy.Image(ctx, policyContext, dst, src, copyOptions)
		return copyErr
	}, retryOptions)
	if err != nil {
		return "", fmt.Errorf("publish image: %w", err)
	}
	named, err := reference.ParseNormalizedNamed(destination)
	if err != nil {
		return "", err
	}
	return reference.TrimNamed(named).String() + "@" + root.Digest.String(), nil
}

func nativeRetryOptions(opts Options) (*retry.Options, error) {
	return oci.RegistryRetryOptions(oci.Options{
		Retry: opts.Retry, RetrySet: opts.RetrySet, RetryDelay: opts.RetryDelay,
	})
}

func publishStoredImage(ctx context.Context, imageID string, root v1.Descriptor, destination string, opts Options) (string, error) {
	transferOptions, cleanup, err := storedTransferOptions(imageID, root.Digest, opts)
	if err != nil {
		return "", err
	}
	defer cleanup()
	transferOptions.RegistryDestination = destination
	repository, err := buildah.TransferStoredImageSupervised(ctx, transferOptions)
	if err != nil {
		return "", fmt.Errorf("publish stored image: %w", err)
	}
	return repository + "@" + root.Digest.String(), nil
}

func signedTransferSystemContext(authFile, certDir, registriesDir string, opts Options) (*types.SystemContext, error) {
	system := &types.SystemContext{
		AuthFilePath:         authFile,
		DockerCertPath:       certDir,
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

func signStoredImage(ctx context.Context, imageID string, manifest digest.Digest, name string, opts Options) error {
	transferOptions, cleanup, err := storedTransferOptions(imageID, manifest, opts)
	if err != nil {
		return err
	}
	defer cleanup()
	transferOptions.LocalName = name
	if _, err := buildah.TransferStoredImageSupervised(ctx, transferOptions); err != nil {
		return fmt.Errorf("sign local image %s: %w", name, err)
	}
	return nil
}

func storedTransferOptions(imageID string, manifest digest.Digest, opts Options) (buildah.StoredTransferOptions, func(), error) {
	storeOptions, err := nativeStoreOptions(opts)
	if err != nil {
		return buildah.StoredTransferOptions{}, func() {}, err
	}
	passphraseFile := opts.Signing.PassphraseFile
	cleanup := func() {}
	if opts.Signing.SigstorePrivateKeyFile != "" && passphraseFile == "" {
		if password := os.Getenv("COSIGN_PASSWORD"); password != "" {
			file, err := os.CreateTemp("", "coopr-signing-passphrase-*")
			if err != nil {
				return buildah.StoredTransferOptions{}, cleanup, fmt.Errorf("create signing passphrase file: %w", err)
			}
			passphraseFile = file.Name()
			cleanup = func() { _ = os.Remove(passphraseFile) }
			if err := file.Chmod(0o600); err != nil {
				_ = file.Close()
				cleanup()
				return buildah.StoredTransferOptions{}, func() {}, err
			}
			if _, err := file.WriteString(password); err != nil {
				_ = file.Close()
				cleanup()
				return buildah.StoredTransferOptions{}, func() {}, err
			}
			if err := file.Close(); err != nil {
				cleanup()
				return buildah.StoredTransferOptions{}, func() {}, err
			}
		}
	}
	return buildah.StoredTransferOptions{
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
		SigstorePrivateKeyFile: opts.Signing.SigstorePrivateKeyFile,
		SigningPassphraseFile:  passphraseFile,
	}, cleanup, nil
}

func (options SigningOptions) passphrases() ([]byte, string, error) {
	if options.PassphraseFile != "" {
		value, err := readPassphraseFile(options.PassphraseFile)
		if err != nil {
			return nil, "", err
		}
		if options.SignBy != "" {
			return nil, string(value), nil
		}
		return value, "", nil
	}
	if options.SigstorePrivateKeyFile != "" {
		return []byte(os.Getenv("COSIGN_PASSWORD")), "", nil
	}
	return nil, "", nil
}

func readPassphraseFile(path string) ([]byte, error) {
	value, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read signing passphrase file: %w", err)
	}
	value, _, _ = bytes.Cut(value, []byte("\n"))
	value = bytes.TrimSuffix(value, []byte("\r"))
	return value, nil
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
