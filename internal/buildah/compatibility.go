package buildah

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"coopr/internal/planner"
	"github.com/containerd/platforms"
	"golang.org/x/sys/unix"
)

const maxCompatibilityMetadataBytes = 1 << 20

const defaultImagePath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// CheckCompatibility validates every retained extend root against a mounted
// caller rootfs. Requirements have already been expanded and normalized by the
// planner. rootfs may be empty when the requirements only inspect the selected
// platform.
func CheckCompatibility(rootfs string, config json.RawMessage, platform string, stages []planner.Stage) error {
	selected, err := platforms.Parse(platform)
	if err != nil {
		return fmt.Errorf("parse caller platform %q: %w", platform, err)
	}
	selected = platforms.Normalize(selected)
	if selected.OS != "linux" {
		return fmt.Errorf("caller platform must be Linux, got %q", platform)
	}

	needsRootfs := false
	for _, stage := range stages {
		if len(stage.Requirements) == 0 {
			continue
		}
		if err := validateStageRequirements(stage, selected.Architecture); err != nil {
			return err
		}
		needsRootfs = needsRootfs || len(stage.Requirements["distro"]) != 0 || len(stage.Requirements["package-manager"]) != 0
	}
	if !needsRootfs {
		return nil
	}
	if rootfs == "" {
		return errors.New("component requires caller filesystem, but caller is scratch")
	}
	if !filepath.IsAbs(rootfs) {
		return errors.New("caller rootfs must be an absolute path")
	}

	rootFD, err := unix.Open(rootfs, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open caller rootfs: %w", err)
	}
	defer func() { _ = unix.Close(rootFD) }()

	var release map[string]string
	for _, stage := range stages {
		requirements := stage.Requirements
		if distros := requirements["distro"]; len(distros) != 0 {
			if release == nil {
				release, err = readRootfsOSRelease(rootFD)
				if err != nil {
					return fmt.Errorf("extend stage %s: %w", stage.ID, err)
				}
			}
			if !slices.Contains(distros, release["ID"]) {
				return fmt.Errorf("extend stage %s requires one of distros %q, caller has %q", stage.ID, distros, release["ID"])
			}
			if versions := requirements["distro-version"]; len(versions) != 0 && !slices.Contains(versions, release["VERSION_ID"]) {
				return fmt.Errorf("extend stage %s requires one of distro-versions %q, caller has %q", stage.ID, versions, release["VERSION_ID"])
			}
		}
		if managers := requirements["package-manager"]; len(managers) != 0 {
			if err := checkRootfsPackageManagers(rootFD, config, managers); err != nil {
				return fmt.Errorf("extend stage %s: %w", stage.ID, err)
			}
		}
	}
	return nil
}

func validateStageRequirements(stage planner.Stage, selectedArchitecture string) error {
	for key, values := range stage.Requirements {
		switch key {
		case "architecture", "distro", "distro-version", "package-manager":
		default:
			return fmt.Errorf("extend stage %s has unknown compatibility requirement %q", stage.ID, key)
		}
		if len(values) == 0 {
			return fmt.Errorf("extend stage %s %s has no allowed values", stage.ID, key)
		}
		for _, value := range values {
			if strings.TrimSpace(value) == "" || strings.ContainsAny(value, "\r\n\x00") {
				return fmt.Errorf("extend stage %s %s is empty or invalid", stage.ID, key)
			}
			if key == "package-manager" {
				if err := validatePackageManager(value); err != nil {
					return fmt.Errorf("extend stage %s: %w", stage.ID, err)
				}
			}
		}
	}
	if len(stage.Requirements["distro-version"]) != 0 && len(stage.Requirements["distro"]) == 0 {
		return fmt.Errorf("extend stage %s distro-version requires distro", stage.ID)
	}
	architectures := stage.Requirements["architecture"]
	if len(architectures) == 0 {
		return nil
	}
	for _, architecture := range architectures {
		if strings.Contains(architecture, "/") {
			return fmt.Errorf("extend stage %s architecture must be a single OCI architecture, got %q", stage.ID, architecture)
		}
		required, err := platforms.Parse("linux/" + architecture)
		if err != nil {
			return fmt.Errorf("extend stage %s invalid architecture %q: %w", stage.ID, architecture, err)
		}
		if platforms.Normalize(required).Architecture == selectedArchitecture {
			return nil
		}
	}
	return fmt.Errorf("extend stage %s requires one of architectures %q, target is %q", stage.ID, architectures, selectedArchitecture)
}

func readRootfsOSRelease(rootFD int) (map[string]string, error) {
	for _, filename := range []string{"/etc/os-release", "/usr/lib/os-release"} {
		data, err := readRootfsRegularFile(rootFD, filename, maxCompatibilityMetadataBytes)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("read %s: %w", filename, err)
		}
		return parseRootfsOSRelease(data)
	}
	return nil, errors.New("caller has no /etc/os-release or /usr/lib/os-release")
}

func readRootfsRegularFile(rootFD int, filename string, limit int64) ([]byte, error) {
	fd, err := openRootfsPath(rootFD, filename, unix.O_RDONLY|unix.O_NONBLOCK)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), filename)
	if file == nil {
		unix.Close(fd) //nolint:errcheck
		return nil, errors.New("create file handle")
	}
	defer file.Close() //nolint:errcheck

	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("is not a regular file")
	}
	if info.Size() > limit {
		return nil, fmt.Errorf("exceeds compatibility metadata limit of %d bytes", limit)
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("exceeds compatibility metadata limit of %d bytes", limit)
	}
	return data, nil
}

func openRootfsPath(rootFD int, filename string, flags int) (int, error) {
	fd, err := unix.Openat2(rootFD, strings.TrimPrefix(filename, "/"), &unix.OpenHow{
		Flags:   uint64(flags | unix.O_CLOEXEC),
		Resolve: unix.RESOLVE_IN_ROOT | unix.RESOLVE_NO_MAGICLINKS,
	})
	if errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EINVAL) {
		return -1, fmt.Errorf("secure caller filesystem inspection requires openat2: %w", err)
	}
	return fd, err
}

func parseRootfsOSRelease(data []byte) (map[string]string, error) {
	result := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSuffix(line, "\r")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok || (key != "ID" && key != "VERSION_ID") {
			continue
		}
		decoded, err := decodeRootfsOSReleaseValue(value)
		if err != nil {
			return nil, fmt.Errorf("malformed os-release %s: %w", key, err)
		}
		result[key] = decoded
	}
	return result, nil
}

func decodeRootfsOSReleaseValue(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	quote := byte(0)
	if value[0] == '\'' || value[0] == '"' {
		quote = value[0]
		if len(value) < 2 || value[len(value)-1] != quote {
			return "", errors.New("unterminated quoted value")
		}
		value = value[1 : len(value)-1]
	}
	var out strings.Builder
	for index := 0; index < len(value); index++ {
		character := value[index]
		if character == '\\' {
			index++
			if index >= len(value) || !strings.ContainsRune("$'\"\\`", rune(value[index])) {
				return "", errors.New("invalid escape")
			}
			out.WriteByte(value[index])
			continue
		}
		if character == '$' || character == '`' || character == '\'' || character == '"' {
			return "", errors.New("unescaped shell special character")
		}
		if quote == 0 && !strings.ContainsRune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789._-", rune(character)) {
			return "", errors.New("unquoted special character")
		}
		out.WriteByte(character)
	}
	return out.String(), nil
}

func validatePackageManager(manager string) error {
	if path.IsAbs(manager) {
		return nil
	}
	if strings.Contains(manager, "/") || manager == "." || manager == ".." {
		return fmt.Errorf("invalid package-manager executable %q", manager)
	}
	return nil
}

func checkRootfsPackageManagers(rootFD int, config json.RawMessage, managers []string) error {
	search := ""
	searchResolved := false
	var firstErr error
	for _, manager := range managers {
		candidates := []string{}
		if path.IsAbs(manager) {
			candidates = append(candidates, manager)
		} else {
			if !searchResolved {
				searchResolved = true
				var err error
				search, err = imagePath(config)
				if err != nil {
					firstErr = err
				}
			}
			if search == "" {
				continue
			}
			for _, directory := range strings.Split(search, ":") {
				if path.IsAbs(directory) {
					candidates = append(candidates, path.Join(directory, manager))
				}
			}
		}
		for _, candidate := range candidates {
			fd, err := openRootfsPath(rootFD, candidate, unix.O_PATH)
			if err != nil {
				if errors.Is(err, os.ErrNotExist) || errors.Is(err, unix.ELOOP) {
					continue
				}
				if firstErr == nil {
					firstErr = fmt.Errorf("stat package-manager %s: %w", candidate, err)
				}
				continue
			}
			var stat unix.Stat_t
			statErr := unix.Fstat(fd, &stat)
			closeErr := unix.Close(fd)
			if statErr != nil {
				if firstErr == nil {
					firstErr = fmt.Errorf("stat package-manager %s: %w", candidate, statErr)
				}
				continue
			}
			if closeErr != nil {
				if firstErr == nil {
					firstErr = fmt.Errorf("close package-manager %s: %w", candidate, closeErr)
				}
				continue
			}
			if stat.Mode&unix.S_IFMT == unix.S_IFREG && stat.Mode&0111 != 0 {
				return nil
			}
		}
	}
	if firstErr != nil {
		return firstErr
	}
	return fmt.Errorf("caller has no executable package-manager from %q in image PATH", managers)
}

func imagePath(config json.RawMessage) (string, error) {
	search := defaultImagePath
	if len(strings.TrimSpace(string(config))) == 0 {
		return search, nil
	}
	var image struct {
		Config struct {
			Env []string `json:"Env"`
		} `json:"config"`
	}
	if err := json.Unmarshal(config, &image); err != nil {
		return "", fmt.Errorf("decode caller PATH: %w", err)
	}
	for _, entry := range image.Config.Env {
		if strings.HasPrefix(entry, "PATH=") {
			search = strings.TrimPrefix(entry, "PATH=")
		}
	}
	return search, nil
}
