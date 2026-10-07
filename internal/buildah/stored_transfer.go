package buildah

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"coopr/internal/oci"
	"github.com/distribution/reference"
	"github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"go.podman.io/common/libimage/manifests"
	"go.podman.io/common/pkg/retry"
	imagecopy "go.podman.io/image/v5/copy"
	"go.podman.io/image/v5/docker"
	imagereference "go.podman.io/image/v5/docker/reference"
	"go.podman.io/image/v5/manifest"
	"go.podman.io/image/v5/signature"
	imagestorage "go.podman.io/image/v5/storage"
	"go.podman.io/image/v5/types"
	"go.podman.io/storage"
	"go.podman.io/storage/pkg/reexec"
	"go.podman.io/storage/pkg/unshare"
)

const storedTransferWorkerName = "coopr-buildah-stored-transfer"

func init() {
	for _, name := range []string{storedTransferWorkerName, storedTransferWorkerName + "-in-a-user-namespace"} {
		reexec.Register(name, runStoredTransferWorker)
	}
}

// StoredTransferOptions describes a native containers/storage copy. Registry
// publication preserves signatures already attached to the storage image and
// may add a new GPG or sigstore signature in the same operation.
type StoredTransferOptions struct {
	Store                  StoreOptions
	ImageID                string
	ExpectedManifest       digest.Digest
	LocalName              string
	RegistryDestination    string
	AuthFile               string
	CertDir                string
	TLSVerify              *bool
	Credentials            string
	Retry                  uint
	RetrySet               bool
	RetryDelay             time.Duration
	SignaturePolicyPath    string
	SignBy                 string
	SigstorePrivateKeyFile string
	SigningPassphraseFile  string
}

type storedTransferRequest struct {
	Options    StoredTransferOptions `json:"options"`
	ResultPath string                `json:"result_path"`
}

type storedTransferResponse struct {
	Reference string `json:"reference,omitempty"`
	Error     string `json:"error,omitempty"`
}

// TransferStoredImageSupervised runs the native copy inside the same user
// namespace model as builds, so overlay-backed stores are never opened from
// the parent namespace.
func TransferStoredImageSupervised(ctx context.Context, options StoredTransferOptions) (string, error) {
	if ctx == nil {
		return "", errors.New("stored transfer context is nil")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if options.ImageID == "" || (options.LocalName == "") == (options.RegistryDestination == "") {
		return "", errors.New("stored transfer requires an image ID and exactly one destination")
	}
	jobDir, err := os.MkdirTemp("", ".coopr-stored-transfer-*")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(jobDir) }()
	request := storedTransferRequest{Options: options, ResultPath: filepath.Join(jobDir, "result.json")}
	requestPath := filepath.Join(jobDir, "request.json")
	if err := writeWorkerJSON(requestPath, request); err != nil {
		return "", err
	}
	command := reexec.Command(storedTransferWorkerName, requestPath)
	command.Env = append(withoutSigningPassword(os.Environ()), "TMPDIR="+jobDir)
	processErr := runWorkerProcess(ctx, command, workerGrace)
	var response storedTransferResponse
	if err := readWorkerJSON(request.ResultPath, &response); err != nil {
		return "", errors.Join(processErr, err)
	}
	if response.Error != "" {
		return "", errors.New(response.Error)
	}
	if processErr != nil {
		return "", processErr
	}
	return response.Reference, nil
}

func runStoredTransferWorker() {
	unshare.MaybeReexecUsingUserNamespace(false)
	if len(os.Args) != 2 {
		_, _ = fmt.Fprintln(os.Stderr, "invalid stored transfer worker arguments")
		os.Exit(2)
	}
	if err := executeStoredTransferWorker(os.Args[1]); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

func executeStoredTransferWorker(requestPath string) error {
	var request storedTransferRequest
	if err := readWorkerJSON(requestPath, &request); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	result, transferErr := transferStoredImage(ctx, request.Options)
	response := storedTransferResponse{Reference: result}
	if transferErr != nil {
		response.Error = transferErr.Error()
	}
	if err := writeWorkerJSON(request.ResultPath, response); err != nil {
		return errors.Join(transferErr, err)
	}
	return transferErr
}

func transferStoredImage(ctx context.Context, options StoredTransferOptions) (_ string, retErr error) {
	lease, err := acquireStore(options.Store)
	if err != nil {
		return "", err
	}
	defer func() { retErr = errors.Join(retErr, lease.Close()) }()
	baseSource, err := imagestorage.Transport.NewStoreReference(lease.store, nil, options.ImageID)
	if err != nil {
		return "", err
	}
	var src types.ImageReference = baseSource
	var dst types.ImageReference
	localIdentity := ""
	if options.LocalName != "" {
		named, err := imagereference.ParseNormalizedNamed(options.LocalName)
		if err != nil {
			return "", fmt.Errorf("parse local signing identity %q: %w", options.LocalName, err)
		}
		localIdentity = named.String()
	} else {
		dst, err = docker.ParseReference("//" + options.RegistryDestination)
		if err != nil {
			return "", fmt.Errorf("open registry destination %q: %w", options.RegistryDestination, err)
		}
	}
	passphrase, err := readStoredTransferPassphrase(options.SigningPassphraseFile)
	if err != nil {
		return "", err
	}
	system, err := storedTransferSystemContext(options, "")
	if err != nil {
		return "", err
	}
	if options.RegistryDestination != "" {
		registriesDir, err := storedSigstoreAttachmentsConfig(system, dst)
		if err != nil {
			return "", err
		}
		defer func() { retErr = errors.Join(retErr, os.RemoveAll(registriesDir)) }()
		system.RegistriesDirPath = registriesDir
	}
	sourceCleanup := func() error { return nil }
	if localIdentity == "" && options.ExpectedManifest != "" {
		src, sourceCleanup, err = storedCopySourceReference(ctx, lease.store, baseSource, system, options.ImageID, options.ExpectedManifest)
		defer func() { retErr = errors.Join(retErr, sourceCleanup()) }()
		if err != nil {
			return "", err
		}
	}
	if localIdentity != "" {
		if options.ExpectedManifest == "" {
			return "", errors.New("local stored-image signing requires an expected manifest")
		}
		if options.SignBy == "" || options.SigstorePrivateKeyFile != "" {
			return "", errors.New("local stored-image signing requires exactly one GPG key")
		}
		if err := signStoredManifest(ctx, lease.store, baseSource, system, options.ImageID, options.ExpectedManifest, localIdentity, options.SignBy, string(passphrase)); err != nil {
			return "", fmt.Errorf("sign stored image: %w", err)
		}
		return options.LocalName, nil
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
		ImageListSelection:           imagecopy.CopySystemImage,
		SignBy:                       options.SignBy,
		SignBySigstorePrivateKeyFile: options.SigstorePrivateKeyFile,
	}
	if options.SignBy != "" {
		copyOptions.SignPassphrase = string(passphrase)
	} else if options.SigstorePrivateKeyFile != "" {
		copyOptions.SignSigstorePrivateKeyPassphrase = passphrase
	}
	retryOptions, err := oci.RegistryRetryOptions(oci.Options{Retry: options.Retry, RetrySet: options.RetrySet, RetryDelay: options.RetryDelay})
	if err != nil {
		return "", err
	}
	err = retry.IfNecessary(ctx, func() error {
		manifest, copyErr := imagecopy.Image(ctx, policyContext, dst, src, copyOptions)
		if copyErr == nil && options.ExpectedManifest != "" && digest.FromBytes(manifest) != options.ExpectedManifest {
			copyErr = fmt.Errorf("copied stored manifest %s, want %s", digest.FromBytes(manifest), options.ExpectedManifest)
		}
		return copyErr
	}, retryOptions)
	if err != nil {
		return "", fmt.Errorf("copy stored image: %w", err)
	}
	named, err := reference.ParseNormalizedNamed(options.RegistryDestination)
	if err != nil {
		return "", err
	}
	return reference.TrimNamed(named).String(), nil
}

func storedCopySourceReference(ctx context.Context, store storage.Store, base types.ImageReference, system *types.SystemContext, imageID string, selected digest.Digest) (_ types.ImageReference, cleanup func() error, retErr error) {
	source, err := base.NewImageSource(ctx, system)
	if err != nil {
		return nil, func() error { return nil }, err
	}
	defer func() { retErr = errors.Join(retErr, source.Close()) }()
	manifestData, mediaType, err := source.GetManifest(ctx, &selected)
	if err != nil {
		return nil, func() error { return nil }, fmt.Errorf("read selected stored manifest: %w", err)
	}
	if actual := digest.FromBytes(manifestData); actual != selected {
		return nil, func() error { return nil }, fmt.Errorf("stored manifest is %s, want %s", actual, selected)
	}
	platform, err := storedManifestPlatform(ctx, source, manifestData)
	if err != nil {
		return nil, func() error { return nil }, err
	}
	indexData, err := json.Marshal(v1.Index{
		Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageIndex,
		Manifests: []v1.Descriptor{{MediaType: mediaType, Digest: selected, Size: int64(len(manifestData)), Platform: &platform}},
	})
	if err != nil {
		return nil, func() error { return nil }, fmt.Errorf("encode selected stored-image index: %w", err)
	}
	indexDigest := digest.FromBytes(indexData)
	indexKey := storage.ImageDigestManifestBigDataNamePrefix + "-" + indexDigest.String()
	// Retain this deterministic one-leaf index as native image metadata. It is
	// bounded to one immutable entry per selected manifest and lets future
	// exports keep using c/image's multi-format signature API without copying
	// the rootfs or creating a second image record.
	if err := store.SetImageBigData(imageID, indexKey, indexData, manifest.Digest); err != nil {
		return nil, func() error { return nil }, fmt.Errorf("store selected image index: %w", err)
	}
	temporaryDir, err := os.MkdirTemp("", "coopr-native-source-")
	if err != nil {
		return nil, func() error { return nil }, err
	}
	name, err := imagereference.ParseNormalizedNamed("localhost/coopr-transfer/" + filepath.Base(temporaryDir) + ":selected")
	if err != nil {
		_ = os.RemoveAll(temporaryDir)
		return nil, func() error { return nil }, err
	}
	if err := store.AddNames(imageID, []string{name.String()}); err != nil {
		_ = os.RemoveAll(temporaryDir)
		return nil, func() error { return nil }, fmt.Errorf("add temporary selected-image name: %w", err)
	}
	cleanup = func() error {
		// Only the unique resolution name is temporary; the deterministic index
		// above is an intentional, reusable part of the canonical image record.
		return errors.Join(store.RemoveNames(imageID, []string{name.String()}), os.RemoveAll(temporaryDir))
	}
	canonical, err := imagereference.WithDigest(imagereference.TrimNamed(name), indexDigest)
	if err != nil {
		return nil, cleanup, err
	}
	reference, err := imagestorage.Transport.NewStoreReference(store, canonical, imageID)
	if err != nil {
		return nil, cleanup, err
	}
	system.OSChoice = platform.OS
	system.ArchitectureChoice = platform.Architecture
	system.VariantChoice = platform.Variant
	return reference, cleanup, nil
}

func storedManifestPlatform(ctx context.Context, source types.ImageSource, manifestData []byte) (v1.Platform, error) {
	var selectedManifest v1.Manifest
	if err := json.Unmarshal(manifestData, &selectedManifest); err != nil {
		return v1.Platform{}, fmt.Errorf("decode selected stored manifest: %w", err)
	}
	reader, _, err := source.GetBlob(ctx, types.BlobInfo{Digest: selectedManifest.Config.Digest, Size: selectedManifest.Config.Size}, nil)
	if err != nil {
		return v1.Platform{}, fmt.Errorf("read selected stored config: %w", err)
	}
	configData, readErr := io.ReadAll(reader)
	if err := errors.Join(readErr, reader.Close()); err != nil {
		return v1.Platform{}, fmt.Errorf("read selected stored config: %w", err)
	}
	var config v1.Image
	if err := json.Unmarshal(configData, &config); err != nil {
		return v1.Platform{}, fmt.Errorf("decode selected stored config: %w", err)
	}
	platform := v1.Platform{OS: config.OS, Architecture: config.Architecture, Variant: config.Variant}
	if platform.OS == "" || platform.Architecture == "" {
		return v1.Platform{}, errors.New("selected stored config has incomplete platform metadata")
	}
	return platform, nil
}

func signStoredManifest(ctx context.Context, store storage.Store, sourceReference types.ImageReference, system *types.SystemContext, imageID string, selected digest.Digest, identity, keyIdentity, passphrase string) (retErr error) {
	mutationLock, err := manifests.LockerForImage(store, imageID)
	if err != nil {
		return fmt.Errorf("open stored-image signature lock: %w", err)
	}
	mutationLock.Lock()
	defer mutationLock.Unlock()
	previous, err := oci.CaptureStoredSignatures(store, imageID)
	if err != nil {
		return err
	}
	source, err := sourceReference.NewImageSource(ctx, system)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, source.Close()) }()
	manifestData, _, err := source.GetManifest(ctx, &selected)
	if err != nil {
		return err
	}
	if actual := digest.FromBytes(manifestData); actual != selected {
		return fmt.Errorf("stored manifest is %s, want %s", actual, selected)
	}
	defaultManifest, _, err := source.GetManifest(ctx, nil)
	if err != nil {
		return fmt.Errorf("read default stored manifest: %w", err)
	}
	selectedIsDefault := digest.FromBytes(defaultManifest) == selected
	existing, err := oci.StoredSignatureBlobs(previous, selected, selectedIsDefault)
	if err != nil {
		return err
	}
	mechanism, err := signature.NewGPGSigningMechanism()
	if err != nil {
		return fmt.Errorf("initialize GPG signing: %w", err)
	}
	defer func() { retErr = errors.Join(retErr, mechanism.Close()) }()
	created, err := signature.SignDockerManifestWithOptions(manifestData, identity, mechanism, keyIdentity, &signature.SignOptions{Passphrase: passphrase})
	if err != nil {
		return fmt.Errorf("create GPG signature: %w", err)
	}
	if err := oci.WriteStoredManifestSignatures(store, imageID, selected, selectedIsDefault, append(existing, created), previous); err != nil {
		return errors.Join(err, oci.RestoreStoredSignatures(store, imageID, previous))
	}
	return nil
}

// selectedStorageReference binds build-time reads to a specific manifest.
// Registry publication uses storedCopySourceReference instead so c/image keeps
// its native multi-format signature interface.
type selectedStorageReference struct {
	types.ImageReference
	manifest digest.Digest
}

func (reference selectedStorageReference) NewImageSource(ctx context.Context, system *types.SystemContext) (types.ImageSource, error) {
	source, err := reference.ImageReference.NewImageSource(ctx, system)
	if err != nil {
		return nil, err
	}
	return &selectedStorageSource{ImageSource: source, manifest: reference.manifest}, nil
}

type selectedStorageSource struct {
	types.ImageSource
	manifest digest.Digest
}

func (source *selectedStorageSource) selected(instance *digest.Digest) *digest.Digest {
	if instance != nil {
		return instance
	}
	selected := source.manifest
	return &selected
}

func (source *selectedStorageSource) GetManifest(ctx context.Context, instance *digest.Digest) ([]byte, string, error) {
	return source.ImageSource.GetManifest(ctx, source.selected(instance))
}

func (source *selectedStorageSource) GetSignatures(ctx context.Context, instance *digest.Digest) ([][]byte, error) {
	return source.ImageSource.GetSignatures(ctx, source.selected(instance))
}

func (source *selectedStorageSource) LayerInfosForCopy(ctx context.Context, instance *digest.Digest) ([]types.BlobInfo, error) {
	return source.ImageSource.LayerInfosForCopy(ctx, source.selected(instance))
}

func storedTransferSystemContext(options StoredTransferOptions, registriesDir string) (*types.SystemContext, error) {
	authFile, certDir, err := oci.NormalizeRegistryPaths(options.AuthFile, options.CertDir)
	if err != nil {
		return nil, err
	}
	system := &types.SystemContext{
		AuthFilePath: authFile, DockerCertPath: certDir, RegistriesDirPath: registriesDir,
		BigFilesTemporaryDir: os.TempDir(), SignaturePolicyPath: options.SignaturePolicyPath,
	}
	oci.ApplyTLSVerify(system, options.TLSVerify)
	if options.Credentials != "" {
		username, password, _ := strings.Cut(options.Credentials, ":")
		if username == "" {
			return nil, errors.New("registry credentials require a username")
		}
		system.DockerAuthConfig = &types.DockerAuthConfig{Username: username, Password: password}
	}
	return system, nil
}

func readStoredTransferPassphrase(path string) ([]byte, error) {
	if path == "" {
		return nil, nil
	}
	value, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read signing passphrase file: %w", err)
	}
	value, _, _ = bytes.Cut(value, []byte("\n"))
	return bytes.TrimSuffix(value, []byte("\r")), nil
}

func storedSigstoreAttachmentsConfig(system *types.SystemContext, destination types.ImageReference) (string, error) {
	named := destination.DockerReference()
	if named == nil {
		return "", errors.New("sigstore attachments require a named registry destination")
	}
	readBase, err := docker.SignatureStorageBaseURL(system, destination, false)
	if err != nil {
		return "", fmt.Errorf("resolve signature lookaside: %w", err)
	}
	writeBase, err := docker.SignatureStorageBaseURL(system, destination, true)
	if err != nil {
		return "", fmt.Errorf("resolve signature lookaside staging: %w", err)
	}
	readTop, err := storedSignatureTopLevel(readBase, named)
	if err != nil {
		return "", err
	}
	writeTop, err := storedSignatureTopLevel(writeBase, named)
	if err != nil {
		return "", err
	}
	dir, err := os.MkdirTemp("", "coopr-registries.d-*")
	if err != nil {
		return "", err
	}
	namespace := map[string]any{"lookaside": readTop, "use-sigstore-attachments": true}
	if writeTop != readTop {
		namespace["lookaside-staging"] = writeTop
	}
	data, err := json.Marshal(map[string]any{"docker": map[string]any{named.Name(): namespace}})
	if err == nil {
		err = os.WriteFile(filepath.Join(dir, "coopr-signing.yaml"), data, 0o600)
	}
	if err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	return dir, nil
}

func storedSignatureTopLevel(base *url.URL, named reference.Named) (string, error) {
	if base == nil {
		return "", errors.New("signature lookaside URL is nil")
	}
	result := *base
	suffix := "/" + reference.Path(named)
	if !strings.HasSuffix(result.Path, suffix) {
		return "", fmt.Errorf("signature lookaside %q does not end in repository path %q", result.Redacted(), suffix)
	}
	result.Path = strings.TrimSuffix(result.Path, suffix)
	return result.String(), nil
}
