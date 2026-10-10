package build

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"coopr/internal/oci"
	"coopr/internal/transfer"
	"github.com/containerd/platforms"
	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

func outputDestinations(kind oci.Kind, tag string, tags []string, push bool) ([]transfer.Destination, error) {
	values := append([]string(nil), tags...)
	if tag != "" {
		values = append([]string{tag}, values...)
	}
	if push && len(values) == 0 {
		return nil, errors.New("--push requires --tag")
	}
	destinations := make([]transfer.Destination, 0, len(values))
	seen := map[transfer.Destination]bool{}
	for _, value := range values {
		if value == "" {
			return nil, errors.New("--tag requires a nonempty name")
		}
		var destination transfer.Destination
		var err error
		if push {
			destination, err = transfer.ParsePushDestination(value, kind)
		} else {
			destination, err = transfer.ParseDestination(value, kind)
		}
		if err != nil {
			if push {
				return nil, fmt.Errorf("push target: %w", err)
			}
			return nil, err
		}
		if !seen[destination] {
			destinations = append(destinations, destination)
			seen[destination] = true
		}
	}
	return destinations, nil
}

func validateOutputArtifacts(destinations []transfer.Destination, definition, metadata, iid string, extra ...string) ([]string, error) {
	paths := []string{}
	for _, destination := range destinations {
		path, err := archiveOutput(destination, definition)
		if err != nil {
			return nil, err
		}
		if path != "" {
			paths = append(paths, path)
		}
	}
	for _, path := range append([]string{metadata, iid}, extra...) {
		if path == "" {
			continue
		}
		if _, err := archiveOutput(transfer.Destination{Transport: "oci-archive", Name: path}, definition); err != nil {
			return nil, err
		}
		paths = append(paths, path)
	}
	for i, path := range paths {
		for _, other := range paths[:i] {
			same, err := sameDestination(path, other)
			if err != nil {
				return nil, err
			}
			if same {
				return nil, fmt.Errorf("output destinations overlap at %s", path)
			}
		}
	}
	return paths, nil
}

// Preflight file outputs before execution, without replacing existing files.
// Remote destinations can still fail at commit time and are reported separately.
func preflightOutputArtifacts(paths []string, stores ...string) error {
	for _, path := range paths {
		for _, store := range stores {
			outputPath, err := canonicalParentPath(path)
			if err != nil {
				return err
			}
			storePath, err := canonicalParentPath(filepath.Join(store, ".coopr-output-boundary"))
			if err != nil {
				return err
			}
			storePath = filepath.Dir(storePath)
			relative, err := filepath.Rel(storePath, outputPath)
			if err != nil {
				return err
			}
			if relative == "." || relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
				return fmt.Errorf("output %s would overwrite the Coopr store", path)
			}
		}
		if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
			return fmt.Errorf("output %s must be a regular file", path)
		} else if err != nil && !os.IsNotExist(err) {
			return err
		}
		file, err := os.CreateTemp(filepath.Dir(path), ".coopr-output-check-*")
		if err != nil {
			return fmt.Errorf("prepare output %s: %w", path, err)
		}
		closeErr := file.Close()
		removeErr := os.Remove(file.Name())
		if err := errors.Join(closeErr, removeErr); err != nil {
			return err
		}
	}
	return nil
}

type destinationResult struct {
	Destination string `json:"destination"`
	Status      string `json:"status"`
	Reference   string `json:"reference,omitempty"`
	Error       string `json:"error,omitempty"`
}

type outputReport struct {
	References   []string
	Destinations []destinationResult
}

func applyOutputDestinations(ctx context.Context, kind oci.Kind, layout string, root v1.Descriptor, destinations []transfer.Destination, opts transfer.Options) (outputReport, error) {
	return publishDestinations(ctx, kind, root, destinations, func(ctx context.Context, destination transfer.Destination) (string, error) {
		copyOptions := opts
		if destination.Transport != "registry" && (destination.Transport != "local" || opts.Signing.SignBy == "") {
			copyOptions.Signing = transfer.SigningOptions{}
		}
		if kind == oci.Image {
			return transfer.Copy(ctx, kind, root.Digest.String(), destination, copyOptions)
		}
		return transfer.CopyRoot(ctx, kind, layout, root, destination, copyOptions)
	})
}

func publishDestinations(ctx context.Context, kind oci.Kind, root v1.Descriptor, destinations []transfer.Destination, copyTo func(context.Context, transfer.Destination) (string, error)) (outputReport, error) {
	if len(destinations) == 0 {
		return outputReport{References: []string{root.Digest.String()}}, nil
	}
	report := outputReport{Destinations: make([]destinationResult, len(destinations))}
	for i, destination := range destinations {
		report.Destinations[i] = destinationResult{Destination: destination.Transport + ":" + destination.Name, Status: "pending"}
	}
	for i, destination := range destinations {
		result, err := "", ctx.Err()
		if err == nil {
			result, err = copyTo(ctx, destination)
		}
		if err != nil {
			report.Destinations[i].Status = "failed"
			report.Destinations[i].Error = err.Error()
			return report, fmt.Errorf("%s retained as %s; completed destinations %v; copy to %s:%s: %w", kind, root.Digest, report.References, destination.Transport, destination.Name, err)
		}
		report.References = append(report.References, result)
		report.Destinations[i].Status = "complete"
		report.Destinations[i].Reference = result
	}
	return report, nil
}

// Metadata uses the established buildx image keys, with per-platform entries
// for consumers that need the individual manifest and configuration digests.
func finishOutputs(path, iidPath string, root v1.Descriptor, variants []oci.IndexVariant, selections map[string]oci.StoredSelection, report outputReport, publicationErr error, rawIID, nativeID string) error {
	metadataErr := writeOutputMetadata(path, iidPath, root, variants, selections, report, publicationErr, rawIID, nativeID)
	if metadataErr != nil {
		metadataErr = fmt.Errorf("result retained as %s; completed destinations %v; %w", root.Digest, report.References, metadataErr)
	}
	return errors.Join(publicationErr, metadataErr)
}

func writeOutputMetadata(path, iidPath string, root v1.Descriptor, variants []oci.IndexVariant, selections map[string]oci.StoredSelection, report outputReport, publicationErr error, rawIID string, nativeID string) error {
	if path == "" && iidPath == "" && rawIID == "" {
		return nil
	}
	type platformResult struct {
		Platform     string `json:"platform"`
		Digest       string `json:"digest"`
		ConfigDigest string `json:"configDigest,omitempty"`
	}
	entries := make([]platformResult, 0, len(variants))
	for _, variant := range variants {
		key := platforms.Format(platforms.Normalize(variant.Platform))
		entry := platformResult{Platform: key, Digest: variant.Manifest.Digest.String()}
		for _, selection := range selections {
			if selection.Manifest.Digest == variant.Manifest.Digest && len(selection.ConfigData) > 0 {
				entry.ConfigDigest = digest.FromBytes(selection.ConfigData).String()
				break
			}
		}
		entries = append(entries, entry)
	}
	metadata := map[string]any{"containerimage.digest": root.Digest.String(), "containerimage.descriptor": root, "coopr.platforms": entries, "coopr.references": report.References, "coopr.outputs": report.Destinations}
	imageID := "sha256:" + nativeID
	if len(entries) == 1 && entries[0].ConfigDigest != "" {
		metadata["containerimage.config.digest"] = entries[0].ConfigDigest
	}
	var resultErr error
	if iidPath != "" {
		if err := writeResultFile(iidPath, []byte(imageID)); err != nil {
			resultErr = fmt.Errorf("write image ID: %w", err)
		}
	}
	if rawIID != "" {
		if err := writeResultFile(rawIID, []byte(strings.TrimPrefix(imageID, "sha256:"))); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("write raw image ID: %w", err))
		}
	}
	if err := errors.Join(publicationErr, resultErr); err != nil {
		metadata["coopr.outputError"] = err.Error()
	}
	if path != "" {
		data, err := json.MarshalIndent(metadata, "", "  ")
		if err != nil {
			return errors.Join(resultErr, err)
		}
		if writeErr := writeResultFile(path, append(data, '\n')); writeErr != nil {
			err = fmt.Errorf("write result metadata: %w", writeErr)
		}
		resultErr = errors.Join(resultErr, err)
	}
	return resultErr
}

func writeResultFile(path string, data []byte) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".coopr-result-*")
	if err != nil {
		return err
	}
	defer func() { _ = file.Close(); _ = os.Remove(file.Name()) }()
	if _, err := file.Write(data); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}
