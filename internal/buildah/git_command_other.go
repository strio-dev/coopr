//go:build !linux

package buildah

import (
	"context"
	"os/exec"
	"time"
)

func gitCommandContext(ctx context.Context, executable string, arguments ...string) *exec.Cmd {
	command := exec.CommandContext(ctx, executable, arguments...)
	command.WaitDelay = 2 * time.Second
	return command
}
