package apibackend

import (
	"errors"
	"fmt"
	"strings"

	"github.com/containerd/platforms"
	"golang.org/x/sys/unix"
)

// The API exposes one requested default, leaving authored FROM overrides and
// all actual execution to the native definition frontend.
func normalizeBuildPlatform(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	parsed, err := platforms.Parse(strings.TrimSpace(value))
	if err != nil || parsed.OS != "linux" || parsed.Architecture == "" {
		return "", fmt.Errorf("invalid build platform %q: expected one linux/arch[/variant]", value)
	}
	return platforms.Format(platforms.Normalize(parsed)), nil
}

func platformExecutionError(err error, requested string) error {
	if err == nil || (!errors.Is(err, unix.ENOEXEC) && !strings.Contains(strings.ToLower(err.Error()), "exec format error")) {
		return err
	}
	selection := ""
	if requested != "" {
		selection = fmt.Sprintf(" (requested default platform %s)", requested)
	}
	return fmt.Errorf("%w; if this RUN targets a foreign architecture%s, the runner must already provide a compatible binfmt_misc handler such as QEMU; Coopr does not install emulators", err, selection)
}
