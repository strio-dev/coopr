// Package client provides Coopr's versioned Unix-socket execution client.
// It imports no execution engine or native storage packages. The caller owns
// the service process, its lifetime pipe, and the resources named by requests.
package client

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/strio-dev/coopr/api"
)

const (
	maxInfoBytes   = 64 << 10
	maxStatusBytes = api.MaxEventBytes + 4<<10
)

var (
	// ErrUnsupportedVersion means the peer does not implement this API major.
	ErrUnsupportedVersion = errors.New("unsupported Coopr API version")
	// ErrMissingCapability means the peer cannot serve a requested feature.
	ErrMissingCapability = errors.New("missing Coopr API capability")
	// ErrCleanupFailed means backend cleanup failed or could not be confirmed.
	// It takes precedence over ordinary context cancellation.
	ErrCleanupFailed = errors.New("coopr cleanup failed or unconfirmed")
)

// Options controls transport reconciliation, not execution or service lifetime.
type Options struct {
	// CancellationTimeout bounds an independent DELETE acknowledgement after
	// an uncertain execution. Zero selects thirty seconds.
	CancellationTimeout time.Duration
}

// StreamOptions delivers events synchronously, preserving backpressure and
// bounded buffering. Writers and callbacks must return; blocking caller code
// can delay Execute's cancellation handling. Nil consumers discard events
// after protocol validation. Progress support is optional unless negotiated.
// Callers synchronize consumers shared by concurrent executions.
type StreamOptions struct {
	Stdout   io.Writer
	Stderr   io.Writer
	Progress func(api.OperationProgress)
}

// ExecutionError retains the execution identity for later Get/CancelWait when
// an outcome is uncertain. Cause is the primary failure; CleanupError records
// an error during independent reconciliation. An ordinary failed execution in
// an acknowledgement does not by itself imply ErrCleanupFailed.
type ExecutionError struct {
	ID           string
	Cause        error
	CleanupError error
}

func (e *ExecutionError) Error() string {
	return fmt.Sprintf("coopr execution %s: %v", e.ID, errors.Join(e.Cause, e.CleanupError))
}
func (e *ExecutionError) Unwrap() error { return errors.Join(e.Cause, e.CleanupError) }

// HTTPError preserves structured admission errors for errors.As(*api.Error).
type HTTPError struct {
	StatusCode int
	Remote     *api.Error
	Message    string
}

func (e *HTTPError) Error() string {
	if e.Remote != nil {
		return fmt.Sprintf("coopr API HTTP %d: %s", e.StatusCode, e.Remote)
	}
	return fmt.Sprintf("coopr API HTTP %d: %s", e.StatusCode, e.Message)
}
func (e *HTTPError) Unwrap() error {
	if e.Remote == nil {
		return nil
	}
	return remoteError(e.Remote)
}

// Client is safe for concurrent requests. A client belongs to one service
// instance: construct a new one after restarting or replacing that service.
type Client struct {
	http          *http.Client
	cancelTimeout time.Duration
	mu            sync.Mutex
	info          *api.Info
}

// New creates a transport for an absolute Unix socket. It performs no I/O.
// Info or Negotiate can be used during caller-owned service readiness polling.
func New(socket string, options Options) (*Client, error) {
	if !filepath.IsAbs(socket) {
		return nil, errors.New("coopr socket must be absolute")
	}
	if options.CancellationTimeout < 0 {
		return nil, errors.New("coopr cancellation timeout must not be negative")
	}
	if options.CancellationTimeout == 0 {
		options.CancellationTimeout = 30 * time.Second
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	return &Client{
		http: &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}},
		cancelTimeout: options.CancellationTimeout,
	}, nil
}

// CloseIdleConnections releases idle transport resources. It neither cancels
// executions nor shuts down the service.
func (c *Client) CloseIdleConnections() { c.http.CloseIdleConnections() }

// Info refreshes the identity and capability snapshot. Binary Version is
// informational: compatibility depends on APIVersion and capabilities.
// An unsuccessful refresh invalidates any previously accepted snapshot.
func (c *Client) Info(ctx context.Context) (info api.Info, retErr error) {
	defer func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.info = nil
		if retErr == nil {
			copy := copyInfo(info)
			c.info = &copy
		}
	}()
	if err := c.getJSON(ctx, http.MethodGet, "/v1/info", &info, maxInfoBytes); err != nil {
		return info, err
	}
	if info.APIVersion != api.Version {
		return info, fmt.Errorf("%w %d (need %d)", ErrUnsupportedVersion, info.APIVersion, api.Version)
	}
	return info, nil
}

// Negotiate requires only the selected features, reusing accepted Info for the
// same service instance. Call Info explicitly to refresh it. Unknown capability
// names and additive response fields do not force lockstep upgrades.
func (c *Client) Negotiate(ctx context.Context, required ...string) (api.Info, error) {
	info, err := c.acceptedInfo(ctx)
	if err == nil {
		err = requireCapabilities(info, required)
	}
	return info, err
}

func copyInfo(info api.Info) api.Info {
	info.Capabilities = append([]string(nil), info.Capabilities...)
	return info
}

func requireCapabilities(info api.Info, required []string) error {
	available := make(map[string]bool, len(info.Capabilities))
	for _, capability := range info.Capabilities {
		available[capability] = true
	}
	for _, capability := range required {
		if !available[capability] {
			return fmt.Errorf("%w %q", ErrMissingCapability, capability)
		}
	}
	return nil
}

func (c *Client) require(ctx context.Context, request api.Request) error {
	required, err := requestCapabilities(request)
	if err != nil {
		return err
	}
	_, err = c.Negotiate(ctx, required...)
	return err
}

func (c *Client) acceptedInfo(ctx context.Context) (api.Info, error) {
	c.mu.Lock()
	var info api.Info
	accepted := c.info != nil
	if accepted {
		info = copyInfo(*c.info)
	}
	c.mu.Unlock()
	if !accepted {
		return c.Info(ctx)
	}
	return info, nil
}

func requestCapabilities(request api.Request) ([]string, error) {
	var operation string
	switch request.Kind {
	case "execute":
		operation = api.CapabilityExecute
	case "build":
		operation = api.CapabilityBuild
	case "component":
		operation = api.CapabilityComponent
	case "component-load":
		operation = api.CapabilityComponentLoad
	case "publish":
		operation = api.CapabilityPublish
	default:
		return nil, fmt.Errorf("unsupported Coopr request kind %q", request.Kind)
	}
	required := []string{operation}
	if request.Options.Network != "" && (request.Kind == "execute" || request.Kind == "build" || request.Kind == "component") {
		required = append(required, api.CapabilityRunNetwork)
	}
	if request.ComponentLoad != nil && request.ComponentLoad.Name != "" {
		required = append(required, api.CapabilityComponentLoadName)
	}
	if request.Publish != nil && (strings.HasPrefix(request.Publish.Source, "oci:") || request.Publish.SourceDigest != "") {
		required = append(required, api.CapabilityPublishOCILayout)
	}
	if request.Build != nil && request.Build.Platform != "" && (request.Kind == "build" || request.Kind == "component") {
		required = append(required, api.CapabilityBuildPlatform)
	}
	if len(request.Options.Cache.RegistryFrom) != 0 || len(request.Options.Cache.RegistryTo) != 0 {
		required = append(required, api.CapabilityRegistryCache)
	}
	named := len(request.Options.Contexts) != 0
	runMountsV2 := request.Options.Cache.MountDir != ""
	if request.Program != nil {
		for _, state := range request.Program.States {
			named = named || state.From.Context != ""
			for _, operation := range state.Operations {
				named = named || operation.From.Context != ""
				for _, mount := range operation.Mounts {
					named = named || mount.From.Context != ""
					runMountsV2 = runMountsV2 || mount.HostPath != "" || mount.From.Context != "" || mount.From.Image != ""
				}
			}
		}
	}
	if named {
		required = append(required, api.CapabilityNamedContexts)
	}
	if runMountsV2 {
		required = append(required, api.CapabilityRunMountsV2)
	}
	return required, nil
}

// Execute invokes one typed execution, native build/component, or publication
// request. It never retries POST. A result may be useful alongside an error,
// especially when publication completed only some destinations.
func (c *Client) Execute(ctx context.Context, request api.Request, streams StreamOptions) (api.Result, error) {
	if err := ctx.Err(); err != nil {
		return api.Result{}, err
	}
	if err := c.require(ctx, request); err != nil {
		return api.Result{}, err
	}
	data, err := json.Marshal(request)
	if err != nil {
		return api.Result{}, err
	}
	if len(data) > api.MaxRequestBytes {
		return api.Result{}, errors.New("coopr request exceeds size limit")
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return api.Result{}, err
	}
	return c.invoke(ctx, hex.EncodeToString(random[:]), data, streams)
}

func (c *Client) invoke(ctx context.Context, id string, data []byte, streams StreamOptions) (out api.Result, retErr error) {
	requestCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	confirmed := false
	defer func() {
		if !confirmed {
			cancel()
			cleanupCtx, stop := context.WithTimeout(context.Background(), c.cancelTimeout)
			defer stop()
			_, cleanupErr := c.CancelWait(cleanupCtx, id)
			retErr = &ExecutionError{ID: id, Cause: retErr, CleanupError: cleanupErr}
		} else if retErr != nil {
			retErr = &ExecutionError{ID: id, Cause: retErr}
		}
	}()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, endpoint(id), bytes.NewReader(data))
	if err != nil {
		return out, err
	}
	req.Header.Set("Content-Type", "application/json")
	// Keep POST single-use, including transport-level body replay.
	req.GetBody = nil
	response, err := c.http.Do(req)
	if err != nil {
		return out, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		// These documented admission refusals precede registration. In
		// particular, DELETE after duplicate would cancel someone else's ID.
		confirmed = response.StatusCode == http.StatusConflict || response.StatusCode == http.StatusTooManyRequests || response.StatusCode == http.StatusServiceUnavailable
		return out, responseError(response)
	}
	out, retErr = consume(response.Body, streams)
	var terminal *terminalError
	if retErr == nil || errors.As(retErr, &terminal) {
		confirmed = true
		if terminal != nil {
			retErr = terminal.error
		}
	}
	return out, retErr
}

func endpoint(id string) string { return "http://coopr/v1/executions/" + id }

func validID(id string) bool {
	if len(id) != 32 {
		return false
	}
	return strings.Trim(id, "abcdef0123456789") == ""
}

// Get observes execution status. It does not cancel or acknowledge cleanup.
func (c *Client) Get(ctx context.Context, id string) (api.Status, error) {
	return c.status(ctx, http.MethodGet, id)
}

// CancelWait cancels exactly id and waits for backend cleanup acknowledgement.
// A canceled unknown ID is a successful tombstone acknowledgement. An ordinary
// execution failure is returned without ErrCleanupFailed; failed or uncertain
// cleanup retains that sentinel. Retry this method with ExecutionError.ID if
// an earlier independent acknowledgement could not be confirmed.
func (c *Client) CancelWait(ctx context.Context, id string) (api.Status, error) {
	status, err := c.status(ctx, http.MethodDelete, id)
	if err != nil {
		return status, errors.Join(ErrCleanupFailed, err)
	}
	if status.State == "running" {
		return status, errors.Join(ErrCleanupFailed, errors.New("coopr cleanup is not complete"))
	}
	if status.Error != nil && status.Error.Code != "canceled" {
		return status, remoteError(status.Error)
	}
	return status, nil
}

func (c *Client) status(ctx context.Context, method, id string) (api.Status, error) {
	var status api.Status
	if !validID(id) {
		return status, errors.New("coopr execution ID must be 32 lowercase hexadecimal characters")
	}
	if err := c.getJSON(ctx, method, "/v1/executions/"+id, &status, maxStatusBytes); err != nil {
		return status, err
	}
	if err := validateStatus(status, id); err != nil {
		return status, err
	}
	return status, nil
}

func (c *Client) getJSON(ctx context.Context, method, path string, out any, limit int64) error {
	req, err := http.NewRequestWithContext(ctx, method, "http://coopr"+path, nil)
	if err != nil {
		return err
	}
	response, err := c.http.Do(req)
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusOK {
		return responseError(response)
	}
	data, err := readResponse(response, limit)
	if err != nil {
		return err
	}
	_, err = decodeObject(data, out)
	return err
}

func readResponse(response *http.Response, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	err = errors.Join(err, response.Body.Close())
	if len(data) > int(limit) {
		err = errors.Join(err, errors.New("coopr JSON response exceeds size limit"))
	}
	return data, err
}

func responseError(response *http.Response) error {
	data, err := readResponse(response, maxInfoBytes)
	if err != nil {
		return errors.Join(&HTTPError{StatusCode: response.StatusCode}, err)
	}
	var remote api.Error
	if _, err := decodeObject(data, &remote); err == nil && validError(&remote) {
		return &HTTPError{StatusCode: response.StatusCode, Remote: &remote}
	}
	return &HTTPError{StatusCode: response.StatusCode, Message: strings.TrimSpace(string(data))}
}

func remoteError(failure *api.Error) error {
	switch failure.Code {
	case "canceled":
		return errors.Join(context.Canceled, failure)
	case "cleanup_failed":
		return errors.Join(ErrCleanupFailed, failure)
	default:
		return failure
	}
}

// LoadComponent verifies and imports an OCI component archive into the explicit local store.
func (c *Client) LoadComponent(ctx context.Context, load api.ComponentLoad, streams StreamOptions) (api.Result, error) {
	return c.Execute(ctx, api.Request{Kind: "component-load", ComponentLoad: &load}, streams)
}
