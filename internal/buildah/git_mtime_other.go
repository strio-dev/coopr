//go:build !linux

package buildah

import (
	"errors"
	"time"
)

func resetGitCheckoutMTimes(string, time.Time) error {
	return errors.New("Git commit-time normalization requires Linux")
}
