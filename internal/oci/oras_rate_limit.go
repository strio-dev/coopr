package oci

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	orasretry "oras.land/oras-go/v2/registry/remote/retry"
)

// orasRateLimitTransport preserves native registry throttling independently of
// configured operation retries. Requests with payloads use their replay budget.
type orasRateLimitTransport struct {
	base http.RoundTripper
	host string
}

func (t orasRateLimitTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	original := request
	for original.Response != nil && original.Response.Request != nil {
		original = original.Response.Request
	}
	if (request.Body != nil && request.Body != http.NoBody) || original.URL.Host != t.host || !strings.HasPrefix(original.URL.Path, "/v2/") {
		return t.base.RoundTrip(request)
	}
	transport := orasretry.Transport{Base: t.base, Policy: func() orasretry.Policy {
		return &orasretry.GenericPolicy{
			MaxRetry: 4, MaxWait: time.Minute, Backoff: orasRateLimitBackoff(),
			Retryable: func(response *http.Response, err error) (bool, error) {
				return err == nil && response.StatusCode == http.StatusTooManyRequests, nil
			},
		}
	}}
	return transport.RoundTrip(request)
}

func orasRateLimitBackoff() orasretry.Backoff {
	delay := 2 * time.Second
	return func(_ int, response *http.Response) time.Duration {
		header := response.Header.Get("Retry-After")
		if seconds, err := strconv.ParseInt(header, 10, 64); err == nil && seconds >= 0 {
			delay = time.Duration(min(seconds, 60)) * time.Second
		} else if deadline, err := http.ParseTime(header); err == nil {
			if remaining := time.Until(deadline); remaining > 0 {
				delay = remaining
			}
		}
		wait := min(delay, time.Minute)
		delay = wait * 2
		return wait
	}
}
