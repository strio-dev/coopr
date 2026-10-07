package main

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"coopr/internal/buildcontext"
	"github.com/containerd/platforms"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/spf13/cobra"
	"go.podman.io/storage/pkg/archive"
)

func resolveBuildInput(argument, explicitFile string) (file, context string, definitionInContext bool, err error) {
	context = "."
	if explicitFile != "" {
		file = explicitFile
		if argument != "" {
			context = argument
		}
		remoteDefinition, err := isHTTPDefinitionURL(file)
		if err != nil {
			return "", "", false, err
		}
		if remoteDefinition {
			return file, context, false, nil
		}
		if file != "-" && !filepath.IsAbs(file) {
			if _, err := os.Stat(file); os.IsNotExist(err) {
				inContext, err := definitionNeedsContextExtraction(context)
				return file, context, inContext, err
			}
		}
		return file, context, false, nil
	}
	if argument != "" {
		info, statErr := os.Stat(argument)
		isDefinition := statErr == nil && info.Mode().IsRegular() && !archive.IsArchivePath(argument)
		if errors.Is(statErr, os.ErrNotExist) {
			primary, parseErr := buildcontext.ParsePrimary(argument, ".")
			if parseErr != nil {
				return "", "", false, parseErr
			}
			isDefinition = argument != "-" && primary.Kind == buildcontext.Local
		} else if statErr != nil {
			return "", "", false, fmt.Errorf("inspect build input %q: %w", argument, statErr)
		}
		if isDefinition {
			file = argument
			context = filepath.Dir(argument)
			return file, context, false, nil
		}
	}
	return "", "", false, fmt.Errorf("definition file is required: supply a file path or --file with a build context")
}

func isHTTPDefinitionURL(value string) (bool, error) {
	if !strings.HasPrefix(value, "http://") && !strings.HasPrefix(value, "https://") {
		return false, nil
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return false, fmt.Errorf("parse definition URL: %w", err)
	}
	if parsed.Host == "" {
		return false, fmt.Errorf("definition URL %q has no host", parsed.Redacted())
	}
	return true, nil
}

func definitionNeedsContextExtraction(context string) (bool, error) {
	if context == "-" {
		return true, nil
	}
	primary, err := buildcontext.ParsePrimary(context, ".")
	if err != nil {
		return false, err
	}
	return primary.Kind != buildcontext.Local || archive.IsArchivePath(primary.Path), nil
}

func readBuildArgFiles(paths, explicit []string) (map[string]string, error) {
	result := make(map[string]string)
	values := namedValues{values: result}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read build argument file %s: %w", path, err)
		}
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSuffix(line, "\r")
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			if err := values.Set(line); err != nil {
				return nil, fmt.Errorf("%s: %w", path, err)
			}
		}
	}
	for _, value := range explicit {
		if err := values.Set(value); err != nil {
			return nil, fmt.Errorf("build argument %q: %w", value, err)
		}
	}
	return result, nil
}

func buildPlatform(cmd *cobra.Command, platform, osName, arch, variant string) (string, error) {
	aliases := cmd.Flags().Changed("os") || cmd.Flags().Changed("arch") || cmd.Flags().Changed("variant")
	if aliases && cmd.Flags().Changed("platform") {
		return "", fmt.Errorf("--platform cannot be used with --os, --arch, or --variant")
	}
	if !aliases {
		return platform, nil
	}
	if osName == "" {
		osName = runtime.GOOS
	}
	if arch == "" {
		arch = runtime.GOARCH
	}
	return platforms.Format(platforms.Normalize(v1.Platform{OS: osName, Architecture: arch, Variant: variant})), nil
}

func defaultBuildFormat() string {
	if format := os.Getenv("BUILDAH_FORMAT"); format != "" {
		return format
	}
	return "oci"
}
