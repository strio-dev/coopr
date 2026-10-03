package oci

import (
	"fmt"
	"time"

	commonconfig "go.podman.io/common/pkg/config"
	"go.podman.io/common/pkg/retry"
)

// RegistryRetryOptions resolves explicit retry controls or the conventional
// containers.conf defaults used by every native registry operation.
func RegistryRetryOptions(options Options) (*retry.Options, error) {
	maxRetry := options.Retry
	delay := options.RetryDelay
	if !options.RetrySet {
		config, err := commonconfig.Default()
		if err != nil {
			return nil, fmt.Errorf("load containers.conf registry retry settings: %w", err)
		}
		maxRetry = config.Engine.Retry
		if delay == 0 && config.Engine.RetryDelay != "" {
			delay, err = time.ParseDuration(config.Engine.RetryDelay)
			if err != nil {
				return nil, fmt.Errorf("parse containers.conf retry_delay: %w", err)
			}
		}
	}
	return &retry.Options{MaxRetry: int(maxRetry), Delay: delay}, nil
}
