package buildah

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/strio-dev/coopr/api"
	upstream "go.podman.io/buildah"
	"go.podman.io/buildah/copier"
	"go.podman.io/storage"
	"go.podman.io/storage/pkg/idtools"
	"go.podman.io/storage/pkg/unshare"
)

// ValidateFileExports admits literal container paths and disjoint host outputs.
// Publication still requires a fresh destination after confirmed worker cleanup.
func ValidateFileExports(exports []api.FileExport) error {
	if len(exports) > 64 {
		return errors.New("at most 64 selected file exports are supported")
	}
	for i, export := range exports {
		if !path.IsAbs(export.Source) || strings.ContainsAny(export.Source, "\x00*?[") {
			return fmt.Errorf("export %d source must be a literal absolute container path", i)
		}
		for _, part := range strings.Split(export.Source, "/") {
			if part == ".." {
				return fmt.Errorf("export %d source escapes its container root", i)
			}
		}
		if !filepath.IsAbs(export.Destination) || strings.ContainsRune(export.Destination, 0) {
			return fmt.Errorf("export %d destination must be an absolute host path", i)
		}
		for _, previous := range exports[:i] {
			if pathsOverlap(previous.Destination, export.Destination) {
				return errors.New("selected export destinations overlap")
			}
		}
	}
	return nil
}

// ValidateFileExportResources rejects selected outputs overlapping caller-owned inputs.
func ValidateFileExportResources(exports []api.FileExport, resources ...string) error {
	if err := ValidateFileExports(exports); err != nil {
		return err
	}
	for _, export := range exports {
		destination, err := canonicalStorePath(export.Destination)
		if err != nil {
			return err
		}
		for _, resource := range resources {
			if resource == "" {
				continue
			}
			resource, err := canonicalStorePath(resource)
			if err != nil {
				return err
			}
			if pathsOverlap(destination, resource) {
				return errors.New("selected export overlaps an execution resource or output")
			}
		}
	}
	return nil
}

func skippedFileExports(exports []api.FileExport, reason string) []api.ExportResult {
	results := make([]api.ExportResult, len(exports))
	for i, export := range exports {
		results[i] = api.ExportResult{State: export.State, Source: export.Source, Destination: export.Destination, Status: "skipped", Error: reason}
	}
	return results
}

func fileExportsForStage(exports []api.FileExport, stage, output string) (selected []api.FileExport, indices []int) {
	for i, export := range exports {
		state := export.State
		if state == "" {
			state = output
		}
		if state == stage {
			selected, indices = append(selected, export), append(indices, i)
		}
	}
	return selected, indices
}

func exportImageFiles(ctx context.Context, store storage.Store, imageID string, exports []api.FileExport, policy timestampPolicy) (results []api.ExportResult, retErr error) {
	if len(exports) == 0 {
		return nil, nil
	}
	root, err := store.MountImage(imageID, nil, "")
	if err != nil {
		return nil, err
	}
	defer func() {
		_, err := store.UnmountImage(imageID, false)
		if err != nil {
			for i := range results {
				results[i].Status, results[i].Error = "failed", "selected image cleanup failed"
			}
			retErr = errors.Join(retErr, err)
		}
	}()
	// MountImage supplies the canonical UID/GID mapping and a read-only mount.
	return exportFilePaths(ctx, root, nil, nil, exports, policy)
}

func exportBuilderFiles(ctx context.Context, store storage.Store, builder *upstream.Builder, exports []api.FileExport, policy timestampPolicy) (results []api.ExportResult, retErr error) {
	container, err := store.Container(builder.ContainerID)
	if err != nil {
		return nil, err
	}
	layer, err := store.Layer(container.LayerID)
	if err != nil {
		return nil, err
	}
	root, err := builder.Mount(builder.MountLabel)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := builder.Unmount(); err != nil {
			for i := range results {
				results[i].Status, results[i].Error = "failed", "selected builder cleanup failed"
			}
			retErr = errors.Join(retErr, err)
		}
	}()
	return exportFilePaths(ctx, root, layer.UIDMap, layer.GIDMap, exports, policy)
}

func exportFilePaths(ctx context.Context, root string, uidMap, gidMap []idtools.IDMap, exports []api.FileExport, policy timestampPolicy) ([]api.ExportResult, error) {
	if err := ValidateFileExports(exports); err != nil {
		return nil, err
	}
	results := make([]api.ExportResult, len(exports))
	var failures error
	for i, export := range exports {
		result := api.ExportResult{State: export.State, Source: export.Source, Destination: export.Destination, Status: "passed"}
		if err := exportFilePath(ctx, root, uidMap, gidMap, export, policy); err != nil {
			result.Status, result.Error = "failed", err.Error()
			failures = errors.Join(failures, fmt.Errorf("export %s: %w", export.Source, err))
		}
		results[i] = result
	}
	return results, failures
}

func exportFilePath(ctx context.Context, root string, uidMap, gidMap []idtools.IDMap, export api.FileExport, policy timestampPolicy) (retErr error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	selected, err := copier.Eval(root, filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(path.Clean(export.Source), "/"))), copier.EvalOptions{})
	if err != nil {
		return fmt.Errorf("resolve selected source: %w", err)
	}
	relative, err := filepath.Rel(root, selected)
	if err != nil || !filepath.IsLocal(relative) {
		return errors.New("selected source escapes container root")
	}
	info, err := os.Lstat(selected)
	if err != nil {
		return err
	}
	if !info.IsDir() && !info.Mode().IsRegular() {
		return errors.New("selected source is not a regular file or directory")
	}
	if _, err := os.Lstat(export.Destination); !errors.Is(err, os.ErrNotExist) {
		if err == nil {
			err = errors.New("selected export destination already exists")
		}
		return err
	}
	if err := os.MkdirAll(filepath.Dir(export.Destination), 0700); err != nil {
		return err
	}
	temporary, err := os.MkdirTemp(filepath.Dir(export.Destination), ".coopr-selected-*")
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, os.RemoveAll(temporary)) }()
	artifact := filepath.Join(temporary, "artifact")
	directory, pattern, destination := filepath.Dir(selected), filepath.Base(selected), temporary
	rename := map[string]string{pattern: "artifact"}
	if info.IsDir() {
		directory, pattern, destination = selected, ".", artifact
		rename = map[string]string{filepath.Base(selected): ".", "/": "."}
		if err := os.Mkdir(artifact, 0700); err != nil {
			return err
		}
	}
	get := copier.GetOptions{UIDMap: uidMap, GIDMap: gidMap, KeepDirectoryNames: true, DisallowWildcard: true}
	put := copier.PutOptions{Rename: rename}
	if unshare.IsRootless() {
		get.StripSetuidBit, get.StripSetgidBit, get.StripXattrs = true, true, true
		owner := &idtools.IDPair{UID: os.Getuid(), GID: os.Getgid()}
		put.ChownDirs, put.ChownFiles = owner, owner
	}
	if epoch := policy.createdEpoch(); epoch != nil {
		timestamp := time.Unix(*epoch, 0).UTC()
		get.Timestamp = &timestamp
	}
	reader, writer := io.Pipe()
	stop := context.AfterFunc(ctx, func() {
		_ = reader.CloseWithError(ctx.Err())
		_ = writer.CloseWithError(ctx.Err())
	})
	defer stop()
	done := make(chan error, 1)
	go func() {
		err := copier.Get(root, directory, get, []string{pattern}, writer)
		done <- errors.Join(err, writer.CloseWithError(err))
	}()
	putErr := copier.Put(destination, destination, put, reader)
	closeErr := reader.CloseWithError(putErr)
	if err := errors.Join(ctx.Err(), putErr, closeErr, <-done); err != nil {
		return err
	}
	if err := validateSelectedExportTree(artifact); err != nil {
		return err
	}
	return promoteCompleteOutput(artifact, export.Destination, true)
}

// stageFileExports keeps native-worker writes private and on the destination's
// filesystem. Only the supervisor promotes receipts after descendant cleanup.
func stageFileExports(exports []api.FileExport) ([]api.FileExport, []string, error) {
	staged := make([]api.FileExport, len(exports))
	var directories []string
	for i, export := range exports {
		if _, err := os.Lstat(export.Destination); !errors.Is(err, os.ErrNotExist) {
			if err == nil {
				err = errors.New("selected export destination already exists")
			}
			return nil, directories, err
		}
		if err := os.MkdirAll(filepath.Dir(export.Destination), 0700); err != nil {
			return nil, directories, err
		}
		directory, err := os.MkdirTemp(filepath.Dir(export.Destination), ".coopr-file-export-*")
		if err != nil {
			return nil, directories, err
		}
		directories = append(directories, directory)
		staged[i] = export
		staged[i].Destination = filepath.Join(directory, "artifact")
	}
	return staged, directories, nil
}

func publishFileExports(ctx context.Context, exports, staged []api.FileExport, receipts []api.ExportResult, cleanupComplete bool) ([]api.ExportResult, error) {
	if !cleanupComplete || ctx.Err() != nil {
		return skippedFileExports(exports, "execution did not confirm uncanceled cleanup"), nil
	}
	if len(receipts) == 0 {
		return skippedFileExports(exports, "selected output state did not produce a filesystem"), nil
	}
	if len(receipts) != len(staged) || len(staged) != len(exports) {
		return skippedFileExports(exports, "invalid selected export receipts"), errors.New("selected export receipt count differs from request")
	}
	// Validate the entire response before publishing any caller-visible output.
	for i, receipt := range receipts {
		if receipt.State != staged[i].State || receipt.Source != staged[i].Source || receipt.Destination != staged[i].Destination ||
			(receipt.Status != "passed" && receipt.Status != "failed" && receipt.Status != "skipped") {
			return skippedFileExports(exports, "invalid selected export receipt"), errors.New("selected export receipt differs from request")
		}
	}
	results := append([]api.ExportResult(nil), receipts...)
	var failures error
	for i := range results {
		results[i].Destination = exports[i].Destination
		if ctx.Err() != nil {
			results[i].Status, results[i].Error = "skipped", "execution canceled"
			failures = errors.Join(failures, ctx.Err())
			continue
		}
		if results[i].Status != "passed" {
			continue
		}
		err := validateSelectedExportTree(staged[i].Destination)
		if err == nil {
			err = promoteCompleteOutput(staged[i].Destination, exports[i].Destination, cleanupComplete)
		}
		if err != nil {
			results[i].Status, results[i].Error = "failed", err.Error()
			failures = errors.Join(failures, fmt.Errorf("publish selected export %s: %w", exports[i].Source, err))
		}
	}
	return results, failures
}

func validateSelectedExportTree(root string) error {
	info, err := os.Lstat(root)
	if err != nil {
		return err
	}
	if info.Mode().IsRegular() {
		return nil
	}
	if !info.IsDir() {
		return errors.New("selected export is not a regular file or directory")
	}
	confined, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer func() { _ = confined.Close() }()
	return filepath.WalkDir(root, func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			link, err := os.Readlink(name)
			if err != nil {
				return err
			}
			relative, err := filepath.Rel(root, name)
			if filepath.IsAbs(link) || err != nil {
				return errors.New("exported symlink escapes selected directory")
			}
			if _, err := confined.Stat(relative); err != nil {
				return fmt.Errorf("exported symlink must resolve inside selected directory: %w", err)
			}
			return nil
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			return errors.New("selected export contains a special file")
		}
		return nil
	})
}
