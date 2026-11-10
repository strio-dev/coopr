//go:build !linux

package buildah

import (
	"context"
	"os/exec"
	"time"
)

func gitCommandContext(ctx context.Context, executable string, arguments ...string) *exec.Cmd {
	// Git comes from the operator's PATH; parsed inputs remain separate argv elements.
	// nosemgrep: cache.semgrep-rules.go.lang.security.audit.dangerous-exec-command
	command := exec.CommandContext(ctx, executable, arguments...)
	command.WaitDelay = 2 * time.Second
	return command
}
