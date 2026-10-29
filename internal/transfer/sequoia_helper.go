package transfer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/coopr/internal/buildah"
	"github.com/strio-dev/coopr/internal/imagestore"
	"github.com/strio-dev/coopr/internal/oci"
	imagecopy "go.podman.io/image/v5/copy"
	"go.podman.io/image/v5/signature"
	"go.podman.io/image/v5/transports/alltransports"
	"go.podman.io/storage/pkg/reexec"
)

const sequoiaTransferWorker = "coopr-sequoia-transfer"

type sequoiaTransferRequest struct {
	Protocol           string
	Operation          string
	Source             string
	Root               v1.Descriptor
	Destination        Destination
	Options            Options
	PreparedPolicy     bool
	Selection          imagecopy.ImageListSelection
	Progress           bool
	CredentialsFile    string
	CosignPasswordFile string
	ResultPath         string
}
type sequoiaTransferResponse struct {
	Reference string
	Error     string
}

func init() {
	for _, name := range []string{sequoiaTransferWorker, sequoiaTransferWorker + "-in-a-user-namespace"} {
		reexec.Register(name, runSequoiaTransferWorker)
	}
}

func needsSequoiaHelper(opts Options) bool {
	return opts.Signing.SequoiaFingerprint != "" && !buildah.NativeSequoia
}

func transferWithSequoiaHelper(ctx context.Context, operation, source string, root v1.Descriptor, destination Destination, opts Options, prepared bool, selection imagecopy.ImageListSelection) (_ string, retErr error) {
	helper, err := buildah.FindSequoiaHelper(ctx)
	if err != nil {
		return "", err
	}
	jobDir, err := os.MkdirTemp("", ".coopr-sequoia-transfer-*")
	if err != nil {
		return "", err
	}
	defer func() { retErr = errors.Join(retErr, os.RemoveAll(jobDir)) }()
	request := sequoiaTransferRequest{Protocol: buildah.SequoiaHelperProtocol, Operation: operation, Source: source, Root: root, Destination: destination, Options: opts, PreparedPolicy: prepared, Selection: selection, Progress: opts.ProgressWriter != nil, ResultPath: filepath.Join(jobDir, "result.json")}
	request.CredentialsFile, err = buildah.StageSigningSecret(jobDir, "credentials", []byte(opts.Credentials))
	if err != nil {
		return "", err
	}
	request.CosignPasswordFile, err = buildah.StageSigningSecret(jobDir, "cosign-password", []byte(os.Getenv("COSIGN_PASSWORD")))
	if err != nil {
		return "", err
	}
	request.Options.Credentials = ""
	request.Options.DecryptionKeys = nil // These copy continuations consume an already prepared image graph.
	path := filepath.Join(jobDir, "request.json")
	if err := buildah.WriteSigningWorkerJSON(path, request); err != nil {
		return "", err
	}
	processErr := buildah.RunSequoiaWorker(ctx, helper, sequoiaTransferWorker, path, jobDir, opts.ProgressWriter)
	var response sequoiaTransferResponse
	if err := buildah.ReadSigningWorkerJSON(request.ResultPath, &response); err != nil {
		return "", errors.Join(processErr, err)
	}
	if response.Error != "" {
		return "", errors.New(response.Error)
	}
	return response.Reference, processErr
}

func runSequoiaTransferWorker() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "invalid Sequoia transfer worker arguments")
		os.Exit(2)
	}
	if err := executeSequoiaTransferWorker(os.Args[1]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

func executeSequoiaTransferWorker(path string) error {
	var request sequoiaTransferRequest
	if err := buildah.ReadSigningWorkerJSON(path, &request); err != nil {
		return err
	}
	if !buildah.NativeSequoia || request.Protocol != buildah.SequoiaHelperProtocol {
		return errors.New("incompatible Sequoia transfer protocol")
	}
	if request.CredentialsFile != "" {
		value, err := os.ReadFile(request.CredentialsFile)
		if err != nil {
			return err
		}
		request.Options.Credentials = string(value)
	}
	if request.CosignPasswordFile != "" {
		value, err := os.ReadFile(request.CosignPasswordFile)
		if err != nil {
			return err
		}
		request.Options.Signing.sigstorePassword = &value
	}
	request.Options.signingOutput = os.Stdout
	if request.Progress {
		request.Options.ProgressWriter = os.Stderr
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	reference, err := executeSequoiaTransfer(ctx, request)
	response := sequoiaTransferResponse{Reference: reference}
	if err != nil {
		response.Error = err.Error()
	}
	return buildah.WriteSigningWorkerJSON(request.ResultPath, response)
}

func executeSequoiaTransfer(ctx context.Context, request sequoiaTransferRequest) (string, error) {
	var policy *signature.Policy
	if request.PreparedPolicy {
		if request.Operation != "copy-destination" && request.Operation != "copy-archive" {
			return "", errors.New("prepared policy is restricted to verified layout copies")
		}
		src, err := imagestore.LayoutReference(request.Source, request.Root)
		if err != nil {
			return "", err
		}
		policy = oci.PreparedImagePolicy(src)
	}
	switch request.Operation {
	case "publish-reference":
		src, err := alltransports.ParseImageName(request.Source)
		if err != nil {
			return "", err
		}
		return publishImageReferenceSelection(ctx, src, request.Root, request.Destination.Name, request.Options, request.Selection)
	case "copy-destination":
		return copyImageDestination(ctx, request.Source, request.Root, request.Destination, request.Options, policy)
	case "copy-archive":
		return copyImageArchive(ctx, request.Source, request.Root, request.Destination, request.Options, policy)
	default:
		return "", errors.New("unknown Sequoia transfer operation")
	}
}

func (opts Options) signingInteractionOutput() io.Writer {
	if opts.signingOutput != nil {
		return opts.signingOutput
	}
	return os.Stderr
}
