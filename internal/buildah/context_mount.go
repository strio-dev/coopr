package buildah

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/identity"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/opencontainers/runtime-spec/specs-go"
	upstream "go.podman.io/buildah"
	"go.podman.io/buildah/copier"
	"go.podman.io/storage"
	"go.podman.io/storage/pkg/idtools"
	"go.podman.io/storage/pkg/unshare"
)

// preparedRunInput keeps a filtered context snapshot alive through both the
// cache lookup and the RUN that consumes it. A hit and a miss therefore use
// the same context input identity.
type preparedRunInput struct {
	operation  Run
	contextDir string
	resolved   []instructionCacheResolvedInput
	complete   bool
	cleanup    func() error
}

func prepareRunInput(ctx context.Context, builder operationBuilder, contextDir string, artifacts []string, operation Run) (_ preparedRunInput, retErr error) {
	operation.executionContext = ctx
	result := preparedRunInput{operation: operation, contextDir: contextDir, complete: true}
	if operation.Stdin != nil {
		return result, nil // A streamed stdin has no stable cache identity.
	}
	if len(operation.Mounts) == 0 {
		return result, nil
	}
	if _, err := serializeRunMounts(operation.Mounts, contextDir); err != nil {
		return preparedRunInput{}, err
	}
	var contextIdentity string
	if hasContextBindMount(operation.Mounts) {
		policy, err := prepareContextPolicyWithIgnore(contextDir, artifacts, operation.ContextIgnoreFile)
		if err != nil {
			return preparedRunInput{}, err
		}
		options, err := policy.apply(upstream.AddAndCopyOptions{})
		if err != nil {
			return preparedRunInput{}, err
		}
		uidMap, gidMap := contextSnapshotIDMaps(builder)
		snapshot, cleanup, err := snapshotContext(options.ContextDir, options.Excludes, uidMap, gidMap)
		if err != nil {
			return preparedRunInput{}, err
		}
		result.contextDir, result.cleanup = snapshot, cleanup
		defer func() {
			if retErr != nil {
				retErr = errors.Join(retErr, cleanup())
			}
		}()
		digester := digest.Canonical.Digester()
		if err := copier.Get(snapshot, snapshot, copier.GetOptions{}, []string{"."}, cacheContextWriter{ctx: ctx, writer: digester.Hash()}); err != nil {
			return preparedRunInput{}, fmt.Errorf("hash filtered RUN context: %w", err)
		}
		contextIdentity = digester.Digest().String()
		result.operation.Mounts = append([]RunMount(nil), operation.Mounts...)
		for index := range result.operation.Mounts {
			if result.operation.Mounts[index].Type == "bind" && !result.operation.Mounts[index].BoundFrom {
				result.operation.Mounts[index].PrivateRelabel = true
			}
		}
	}
	result.resolved = make([]instructionCacheResolvedInput, 0, len(operation.Mounts))
	imageIdentities := make(map[string]string)
	for _, mount := range operation.Mounts {
		var identity instructionCacheResolvedInput
		switch mount.Type {
		case "bind":
			if mount.BoundFrom {
				imageID := mount.Properties["from"]
				rootDigest, found := imageIdentities[imageID]
				if !found {
					var err error
					rootDigest, err = runMountImageRootDigest(ctx, builder, operation.sourceStore, imageID)
					if err != nil {
						return preparedRunInput{}, fmt.Errorf("identify RUN bind source image: %w", err)
					}
					imageIdentities[imageID] = rootDigest
				}
				identity = instructionCacheResolvedInput{Kind: "image-rootfs", Identity: rootDigest}
			} else {
				identity = instructionCacheResolvedInput{Kind: "context", Identity: contextIdentity}
			}
		case "cache":
			from := mount.Properties["from"]
			if mount.BoundFrom {
				rootDigest, found := imageIdentities[from]
				if !found {
					var err error
					rootDigest, err = runMountImageRootDigest(ctx, builder, operation.sourceStore, from)
					if err != nil {
						return preparedRunInput{}, fmt.Errorf("identify RUN cache source image: %w", err)
					}
					imageIdentities[from] = rootDigest
				}
				from = rootDigest
			}
			identity = instructionCacheResolvedInput{Kind: "cache", Identity: mount.Properties["id"] + "\x00" + from}
		case "tmpfs":
			identity = instructionCacheResolvedInput{Kind: "tmpfs", Identity: "ephemeral"}
		case "secret", "ssh":
			// The operation itself carries the authored mount metadata. Keep only
			// a stable type marker here: secret bytes, SSH agent state, and the
			// host source selected by --secret/--ssh are session inputs and must
			// not invalidate an otherwise identical RUN cache key.
			identity = instructionCacheResolvedInput{Kind: mount.Type, Identity: "credential"}
		default:
			return preparedRunInput{}, fmt.Errorf("unsupported RUN cache mount type %q", mount.Type)
		}
		result.resolved = append(result.resolved, identity)
	}
	return result, nil
}

func runMountImageRootDigest(ctx context.Context, _ operationBuilder, store storage.Store, imageID string) (string, error) {
	if store == nil || imageID == "" {
		return "", errors.New("RUN mount has no resolved source image")
	}
	configData, err := packageImageConfig(ctx, store, imageID, nil)
	if err != nil {
		return "", fmt.Errorf("read RUN mount source image config: %w", err)
	}
	var image v1.Image
	if err := json.Unmarshal(configData, &image); err != nil {
		return "", fmt.Errorf("decode RUN mount source image config: %w", err)
	}
	return rootFSIdentity(image.RootFS)
}

func rootFSIdentity(rootFS v1.RootFS) (string, error) {
	if rootFS.Type != "layers" {
		return "", fmt.Errorf("image has unsupported rootfs type %q", rootFS.Type)
	}
	for index, diffID := range rootFS.DiffIDs {
		if err := diffID.Validate(); err != nil {
			return "", fmt.Errorf("image has invalid diff ID %d: %w", index, err)
		}
	}
	chainID := identity.ChainID(rootFS.DiffIDs)
	if chainID == "" {
		return "empty", nil
	}
	return chainID.String(), nil
}

type cacheContextWriter struct {
	ctx    context.Context
	writer io.Writer
}

func (writer cacheContextWriter) Write(data []byte) (int, error) {
	if err := writer.ctx.Err(); err != nil {
		return 0, err
	}
	return writer.writer.Write(data)
}

func applyRunWithFilteredContext(builder operationBuilder, contextDir string, artifacts []string, operation Run) (retErr error) {
	if !hasContextBindMount(operation.Mounts) {
		return operation.apply(builder, contextDir)
	}
	// Validate before touching the filesystem so malformed mount paths fail for
	// their authored reason instead of an incidental context snapshot error.
	if _, err := serializeRunMounts(operation.Mounts, contextDir); err != nil {
		return err
	}
	policy, err := prepareContextPolicyWithIgnore(contextDir, artifacts, operation.ContextIgnoreFile)
	if err != nil {
		return err
	}
	options, err := policy.apply(upstream.AddAndCopyOptions{})
	if err != nil {
		return err
	}
	uidMap, gidMap := contextSnapshotIDMaps(builder)
	snapshot, cleanup, err := snapshotContext(options.ContextDir, options.Excludes, uidMap, gidMap)
	if err != nil {
		return err
	}
	defer func() {
		retErr = errors.Join(retErr, cleanup())
	}()
	operation.Mounts = append([]RunMount(nil), operation.Mounts...)
	for index := range operation.Mounts {
		if operation.Mounts[index].Type == "bind" && !operation.Mounts[index].BoundFrom {
			operation.Mounts[index].PrivateRelabel = true
		}
	}
	return operation.apply(builder, snapshot)
}

func hasContextBindMount(mounts []RunMount) bool {
	for _, mount := range mounts {
		if mount.Type == "bind" && !mount.BoundFrom {
			return true
		}
	}
	return false
}

func snapshotContext(contextDir string, excludes []string, uidMap, gidMap []idtools.IDMap) (string, func() error, error) {
	if contextDir == "" {
		return "", nil, errors.New("context snapshot requires a build context")
	}
	rootInfo, err := os.Stat(contextDir)
	if err != nil {
		return "", nil, fmt.Errorf("inspect build context: %w", err)
	}
	temporaryRoot, err := os.MkdirTemp("", "coopr-context-")
	if err != nil {
		return "", nil, fmt.Errorf("create context snapshot root: %w", err)
	}
	cleanup := func() error {
		if err := os.RemoveAll(temporaryRoot); err != nil {
			return fmt.Errorf("remove context snapshot: %w", err)
		}
		return nil
	}
	snapshot := filepath.Join(temporaryRoot, "context")
	if err := os.Mkdir(snapshot, rootInfo.Mode().Perm()); err != nil {
		_ = cleanup()
		return "", nil, fmt.Errorf("create context snapshot: %w", err)
	}
	if stat, ok := rootInfo.Sys().(*syscall.Stat_t); ok && (len(uidMap) != 0 || len(gidMap) != 0) {
		uid, gid := int(stat.Uid), int(stat.Gid)
		if len(uidMap) != 0 {
			uid, err = idtools.RawToHost(uid, uidMap)
			if err != nil {
				_ = cleanup()
				return "", nil, fmt.Errorf("map context root owner: %w", err)
			}
		}
		if len(gidMap) != 0 {
			gid, err = idtools.RawToHost(gid, gidMap)
			if err != nil {
				_ = cleanup()
				return "", nil, fmt.Errorf("map context root group: %w", err)
			}
		}
		if err := os.Chown(snapshot, uid, gid); err != nil {
			_ = cleanup()
			return "", nil, fmt.Errorf("set context snapshot root ownership: %w", err)
		}
	}

	reader, writer := io.Pipe()
	getResult := make(chan error, 1)
	go func() {
		getErr := copier.Get(contextDir, contextDir, copier.GetOptions{
			Excludes: excludes,
		}, []string{"."}, writer)
		getResult <- errors.Join(getErr, writer.CloseWithError(getErr))
	}()
	putErr := copier.Put(snapshot, snapshot, copier.PutOptions{
		UIDMap: uidMap, GIDMap: gidMap, IgnoreDevices: unshare.IsRootless(),
	}, reader)
	if putErr != nil {
		_ = reader.CloseWithError(putErr)
	} else {
		_ = reader.Close()
	}
	getErr := <-getResult
	if err := errors.Join(getErr, putErr); err != nil {
		_ = cleanup()
		return "", nil, fmt.Errorf("snapshot build context: %w", err)
	}
	return snapshot, cleanup, nil
}

func contextSnapshotIDMaps(builder operationBuilder) ([]idtools.IDMap, []idtools.IDMap) {
	native, ok := builder.(nativeBuilder)
	if !ok || native.Builder == nil {
		return nil, nil
	}
	convert := func(mappings []specs.LinuxIDMapping) []idtools.IDMap {
		result := make([]idtools.IDMap, len(mappings))
		for index, mapping := range mappings {
			result[index] = idtools.IDMap{ContainerID: int(mapping.ContainerID), HostID: int(mapping.HostID), Size: int(mapping.Size)}
		}
		return result
	}
	return convert(native.IDMappingOptions.UIDMap), convert(native.IDMappingOptions.GIDMap)
}
