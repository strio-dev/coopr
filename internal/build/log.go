package build

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"coopr/internal/transfer"
)

func normalizeBuildLog(path string, split bool) (string, error) {
	if split && path == "" {
		return "", fmt.Errorf("--logsplit requires --logfile")
	}
	if path == "" {
		return "", nil
	}
	return filepath.Abs(path)
}

func platformLogPath(path, platform string) string {
	return path + "_" + strings.ReplaceAll(platform, "/", "_")
}

func openBuildLog(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
}

func buildWriters(stdout, stderr io.Writer, quiet bool, log *os.File) (io.Writer, io.Writer) {
	if log != nil {
		return log, log
	}
	if quiet {
		return io.Discard, io.Discard
	}
	return stdout, stderr
}

// Only completed named image destinations have actually been tagged.
func logCompletedTags(writer io.Writer, destinations []transfer.Destination, report outputReport) {
	if writer == nil {
		return
	}
	for index, result := range report.Destinations {
		if index >= len(destinations) || result.Status != "complete" {
			continue
		}
		destination := destinations[index]
		if destination.Transport == "registry" && strings.Contains(destination.Name, "@") {
			continue
		}
		switch destination.Transport {
		case "local", "podman", "docker", "registry":
			_, _ = fmt.Fprintf(writer, "Successfully tagged %s\n", destination.Name)
		}
	}
}
