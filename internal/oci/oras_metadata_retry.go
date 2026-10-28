package oci

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	nativeerrcode "github.com/docker/distribution/registry/api/errcode"
	"github.com/docker/distribution/registry/client/auth/challenge"
	"go.podman.io/common/pkg/retry"
	"go.podman.io/image/v5/docker"
	"oras.land/oras-go/v2/registry"
)

// orasMetadataTransport reads registry metadata inside the upstream HTTP retry
// attempt. Blob payloads remain streaming; ORAS owns authentication and parsing.
type orasMetadataTransport struct {
	base       http.RoundTripper
	repository registry.Reference
}

func (t orasMetadataTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := t.base.RoundTrip(request)
	if err != nil || response.StatusCode < 200 || response.StatusCode >= 300 || !t.metadataRequest(request) {
		return response, err
	}
	if _, err := readRegistryResponse(response); err != nil {
		return nil, err
	}
	return response, nil
}

func (t orasMetadataTransport) metadataRequest(request *http.Request) bool {
	// Follow the original request when net/http has followed a redirect. Its
	// metadata body still needs to be read before the attempt can succeed.
	for request.Response != nil && request.Response.Request != nil {
		request = request.Response.Request
	}
	if request.Method != http.MethodGet || request.URL.Host != t.repository.Host() {
		return false
	}
	prefix := "/v2/" + t.repository.Repository
	return strings.HasPrefix(request.URL.Path, prefix+"/manifests/") || strings.HasPrefix(request.URL.Path, prefix+"/referrers/")
}

func readRegistryResponse(response *http.Response) ([]byte, error) {
	body := response.Body
	response.Body = http.NoBody
	data, err := readRegistryBody(body, response.ContentLength)
	if err != nil {
		return nil, err
	}
	response.Body = io.NopCloser(bytes.NewReader(data))
	return data, nil
}

func readRegistryBody(body io.ReadCloser, size int64) ([]byte, error) {
	if size > maxMetadataBytes {
		return nil, errors.Join(fmt.Errorf("registry metadata exceeds %d bytes", maxMetadataBytes), body.Close())
	}
	data, err := io.ReadAll(io.LimitReader(body, maxMetadataBytes+1))
	if err = errors.Join(err, body.Close()); err != nil {
		return nil, err
	}
	if len(data) > maxMetadataBytes {
		return nil, fmt.Errorf("registry metadata exceeds %d bytes", maxMetadataBytes)
	}
	return data, nil
}

// orasReplayTransport supplies standard HTTP replay for the SDK's bounded
// attachment metadata. Unlimited component packages use the file-backed path.
type orasReplayTransport struct {
	base http.RoundTripper
}

func (t orasReplayTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.Body == nil || request.GetBody != nil {
		return t.base.RoundTrip(request)
	}
	if err := request.Context().Err(); err != nil {
		return nil, errors.Join(err, request.Body.Close())
	}
	stop := context.AfterFunc(request.Context(), func() { _ = request.Body.Close() })
	data, err := readRegistryBody(request.Body, request.ContentLength)
	stop()
	if request.Context().Err() != nil {
		return nil, request.Context().Err()
	}
	if err != nil {
		return nil, err
	}
	request = request.Clone(request.Context())
	request.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil }
	request.Body, _ = request.GetBody()
	return t.base.RoundTrip(request)
}

func orasHTTPRetryable(response *http.Response, err error) (bool, error) {
	if err != nil {
		return orasArtifactRetryable(err), nil
	}
	if response.StatusCode < 400 {
		return false, nil
	}
	if response.StatusCode >= 500 {
		return retry.IsErrorRetryable(docker.UnexpectedHTTPStatusError{StatusCode: response.StatusCode}), nil
	}
	data, err := readRegistryResponse(response)
	if err != nil {
		if orasArtifactRetryable(err) {
			return true, nil
		}
		return false, err
	}
	for _, candidate := range challenge.ResponseChallenges(response) {
		if candidate.Scheme == "bearer" && (candidate.Parameters["error"] == "invalid_token" || candidate.Parameters["error"] == "insufficient_scope") {
			return false, nil
		}
	}
	// Native registry error handling also accepts this legacy message shape.
	var details struct {
		Details string `json:"details"`
	}
	if json.Unmarshal(data, &details) == nil && details.Details != "" {
		return nativeHTTPStatusRetryable(response.StatusCode), nil
	}
	var causes nativeerrcode.Errors
	if err := json.Unmarshal(data, &causes); err == nil && len(causes) != 0 {
		// Native response handling returns the first registry error.
		return retry.IsErrorRetryable(causes[0]), nil
	}
	return retry.IsErrorRetryable(docker.UnexpectedHTTPStatusError{StatusCode: response.StatusCode}), nil
}

func nativeHTTPStatusRetryable(status int) bool {
	code := nativeerrcode.ErrorCodeUnknown
	switch status {
	case http.StatusUnauthorized:
		code = nativeerrcode.ErrorCodeUnauthorized
	case http.StatusTooManyRequests:
		code = nativeerrcode.ErrorCodeTooManyRequests
	}
	return retry.IsErrorRetryable(code.WithMessage(""))
}
