package buildah

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	upstream "go.podman.io/buildah"
	storagearchive "go.podman.io/storage/pkg/archive"
	"go.podman.io/storage/pkg/chrootarchive"
	"go.podman.io/storage/pkg/unshare"
)

// FilesystemOutput is a flattened rootfs, distinct from an OCI image archive.
// A stdout destination is staged as a tar file by the supervising caller.
type FilesystemOutput struct {
	Type string
	Path string
}

func ParseFilesystemOutput(value string) (FilesystemOutput, error) {
	if value == "" {
		return FilesystemOutput{}, nil
	}
	if value == "-" {
		return FilesystemOutput{Type: "tar", Path: "-"}, nil
	}
	if !strings.Contains(value, "=") {
		return FilesystemOutput{Type: "local", Path: value}, nil
	}
	fields, err := csv.NewReader(strings.NewReader(value)).Read()
	if err != nil {
		return FilesystemOutput{}, fmt.Errorf("parse output: %w", err)
	}
	options := map[string]string{}
	for _, field := range fields {
		key, val, ok := strings.Cut(field, "=")
		if !ok || val == "" || (key != "type" && key != "dest") {
			return FilesystemOutput{}, fmt.Errorf("invalid output option %q; use type=local|tar,dest=PATH", field)
		}
		if _, duplicate := options[key]; duplicate {
			return FilesystemOutput{}, fmt.Errorf("duplicate output option %q", key)
		}
		options[key] = val
	}
	output := FilesystemOutput{Type: options["type"], Path: options["dest"]}
	if (output.Type != "local" && output.Type != "tar") || output.Path == "" || output.Type == "local" && output.Path == "-" {
		return FilesystemOutput{}, errors.New("output requires type=local|tar,dest=PATH; stdout (-) requires tar")
	}
	return output, nil
}

func exportFilesystem(ctx context.Context, builder *upstream.Builder, output FilesystemOutput, policy timestampPolicy) (retErr error) {
	options := upstream.CommitOptions{}
	policy.apply(&options)
	extract := upstream.ExtractRootfsOptions{}
	if epoch := policy.createdEpoch(); epoch != nil {
		timestamp := time.Unix(*epoch, 0).UTC()
		extract.ForceTimestamp = &timestamp
	}
	if unshare.IsRootless() {
		extract.StripSetuidBit, extract.StripSetgidBit, extract.StripXattrs = true, true, true
	}
	stream, done, err := builder.ExtractRootfs(options, extract)
	if err != nil {
		return fmt.Errorf("extract image filesystem: %w", err)
	}
	defer func() {
		retErr = errors.Join(retErr, stream.Close())
		if done != nil {
			retErr = errors.Join(retErr, <-done)
		}
	}()
	if output.Type == "local" {
		if err := os.MkdirAll(output.Path, 0o755); err != nil {
			return err
		}
		return chrootarchive.Untar(contextReader{ctx: ctx, reader: stream}, output.Path, &storagearchive.TarOptions{NoLchown: unshare.IsRootless()})
	}
	file, err := os.Create(output.Path)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(file, contextReader{ctx: ctx, reader: stream})
	return errors.Join(copyErr, file.Close())
}
