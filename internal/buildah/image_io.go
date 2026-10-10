package buildah

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"coopr/internal/imagestore"
	"coopr/internal/oci"
	"coopr/internal/storeactivity"

	"github.com/containerd/platforms"
	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"go.podman.io/buildah/pkg/cli"
	"go.podman.io/common/libimage"
	"go.podman.io/common/pkg/config"
	imagecopy "go.podman.io/image/v5/copy"
	"go.podman.io/image/v5/manifest"
	"go.podman.io/image/v5/signature"
	imagestorage "go.podman.io/image/v5/storage"
	"go.podman.io/image/v5/transports/alltransports"
	"go.podman.io/image/v5/types"
	"go.podman.io/storage"
)

// WithImageStore holds a store activity lease throughout a native image operation.
func WithImageStore(ctx context.Context, options StoreOptions, use func(storage.Store) error) (retErr error) {
	lease, err := storeactivity.AcquireShared(ctx, ActivityRoots(options, "")...)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, lease.Close()) }()
	return WithStore(options, use)
}

// PullImage uses the same native pull and verified origin handling as build inputs.
func PullImage(ctx context.Context, store StoreOptions, registry oci.Options, reference string, platform v1.Platform) (id string, err error) {
	registry.NativeStore = NativeStoreOptions(store)
	resolver, err := oci.NewResolver(registry)
	if err != nil {
		return "", err
	}
	err = WithImageStore(ctx, store, func(backend storage.Store) error {
		image, err := ResolveImageSource(ctx, resolver, reference, platform, backend, resolver.SystemContext())
		id = image.ImageID
		return err
	})
	return id, err
}

// PullAllTags lets libimage apply native short-name, registry and tag-list policy.
func PullAllTags(ctx context.Context, store StoreOptions, options oci.Options, reference string, platform v1.Platform) (ids []string, err error) {
	err = WithImageStore(ctx, store, func(backend storage.Store) error {
		resolver, err := oci.NewResolver(options)
		if err != nil {
			return err
		}
		system := resolver.SystemContext()
		system.OSChoice = platform.OS
		system.ArchitectureChoice = platform.Architecture
		system.VariantChoice = platform.Variant
		native, err := libimage.RuntimeFromStore(backend, &libimage.RuntimeOptions{SystemContext: system})
		if err != nil {
			return err
		}
		policy, err := config.ParsePullPolicy(options.PullPolicy)
		if err != nil {
			return err
		}
		copied := imageCopyOptions(options, options.ProgressWriter)
		copied.OS = platform.OS
		copied.Architecture = platform.Architecture
		copied.Variant = platform.Variant
		copied.OciDecryptConfig, err = cli.DecryptConfig(options.DecryptionKeys)
		if err != nil {
			return err
		}
		images, pullErr := native.Pull(ctx, reference, policy, &libimage.PullOptions{CopyOptions: copied, AllTags: true})
		for _, image := range images {
			ids = append(ids, image.ID())
		}
		return pullErr
	})
	return ids, err
}

func imageCopyOptions(options oci.Options, writer io.Writer) libimage.CopyOptions {
	copied := libimage.CopyOptions{AuthFilePath: options.AuthFile, CertDirPath: options.CertDir, Credentials: options.Credentials, SignaturePolicyPath: options.SignaturePolicyPath, Writer: writer}
	if options.TLSVerify != nil {
		copied.InsecureSkipTLSVerify = types.NewOptionalBool(!*options.TLSVerify)
	}
	if options.RetrySet {
		copied.MaxRetries = &options.Retry
	}
	if options.RetryDelay != 0 {
		copied.RetryDelay = &options.RetryDelay
	}
	return copied
}

// LoadImages delegates archive and directory detection and import to libimage.
func LoadImages(ctx context.Context, store StoreOptions, path string, options oci.Options, writer io.Writer) (names []string, err error) {
	err = WithImageStore(ctx, store, func(backend storage.Store) error {
		native, err := libimage.RuntimeFromStore(backend, nil)
		if err != nil {
			return err
		}
		copied := imageCopyOptions(options, writer)

		var root v1.Descriptor
		var rootData []byte
		var archiveReference types.ImageReference
		// Native source readers can expose index metadata before native.Load tries
		// to select the host platform. This also permits foreign-only indexes.
		for _, transport := range []string{"oci:", "oci-archive:"} {
			ref, err := alltransports.ParseImageName(transport + path)
			if err != nil {
				continue
			}
			source, err := ref.NewImageSource(ctx, native.SystemContext())
			if err != nil {
				continue
			}
			data, mediaType, readErr := source.GetManifest(ctx, nil)
			closeErr := source.Close()
			if closeErr != nil {
				return closeErr
			}
			if readErr != nil {
				continue
			}
			if manifest.MIMETypeIsMultiImage(mediaType) {
				root = oci.Descriptor(mediaType, data)
				rootData = data
				archiveReference = ref
			}
			break
		}
		if rootData != nil {
			var index v1.Index
			if err := json.Unmarshal(rootData, &index); err != nil {
				return err
			}
			for _, child := range index.Manifests {
				if child.Platform == nil || child.Platform.OS == "unknown" || child.Platform.Architecture == "unknown" {
					continue
				}
				system := native.SystemContext()
				system.OSChoice = child.Platform.OS
				system.ArchitectureChoice = child.Platform.Architecture
				system.VariantChoice = child.Platform.Variant
				native, err = libimage.RuntimeFromStore(backend, &libimage.RuntimeOptions{SystemContext: system})
				if err != nil {
					return err
				}
				copied.OS = child.Platform.OS
				copied.Architecture = child.Platform.Architecture
				copied.Variant = child.Platform.Variant
				break
			}
		}

		names, err = native.Load(ctx, path, &libimage.LoadOptions{CopyOptions: copied})
		if err != nil || rootData == nil {
			return err
		}
		// Import each runnable platform through the native loader, then publish an
		// index of the imported children with the existing native index writer.
		var index v1.Index
		if err := json.Unmarshal(rootData, &index); err != nil {
			return err
		}
		imageIDs := map[digest.Digest]string{}
		tag := ""
		if len(names) == 1 && !strings.HasPrefix(names[0], "sha256:") {
			tag = names[0]
		}
		imported := make([]v1.Descriptor, 0, len(index.Manifests))
		for _, child := range index.Manifests {
			if child.Platform == nil || child.Platform.OS == "unknown" || child.Platform.Architecture == "unknown" {
				continue
			}
			selectedCopy := copied
			selectedCopy.OS = child.Platform.OS
			selectedCopy.Architecture = child.Platform.Architecture
			selectedCopy.Variant = child.Platform.Variant
			loadedID, err := loadArchiveInstance(ctx, backend, archiveReference, child, selectedCopy)
			if err != nil {
				return fmt.Errorf("load archived platform %s: %w", platforms.Format(*child.Platform), err)
			}
			selected, err := oci.ResolveStoredImage(ctx, backend, loadedID, *child.Platform)
			if err != nil {
				return err
			}
			if selected.Selected.Digest != child.Digest {
				return errors.New("loaded platform manifest differs from archived index")
			}
			imageIDs[child.Digest] = selected.StorageImageID
			imported = append(imported, child)
		}
		if len(imported) != len(index.Manifests) {
			// Native image storage cannot represent arbitrary attestation layers.
			// Keep index metadata and imported descriptors, but omit children that
			// were not loaded instead of publishing dangling references to them.
			index.Manifests = imported
			rootData, err = json.Marshal(index)
			if err != nil {
				return err
			}
			root = oci.Descriptor(root.MediaType, rootData)
		}
		id, err := imagestore.FromStore(backend).WriteStoredIndex(ctx, root, rootData, imageIDs, tag)
		if err != nil {
			return err
		}
		if tag == "" {
			names = []string{id}
		}
		return nil
	})
	return names, err
}

// SaveImages uses native archive writers; OCI archive exports use transfer.Copy
// instead so Coopr's selected manifest and complete index remain authoritative.
func SaveImages(ctx context.Context, store StoreOptions, names []string, format, path string, options oci.Options, platform v1.Platform, explicit bool, writer io.Writer, saveOptions libimage.SaveOptions) error {
	return WithImageStore(ctx, store, func(backend storage.Store) error {
		requested := append([]string(nil), names...)
		selectedByID := map[string]*oci.Resolved{}
		var solePlatform v1.Platform
		for i, name := range names {
			choice := platform
			if !explicit {
				available, err := oci.StoredImagePlatforms(ctx, backend, name)
				if err != nil {
					return err
				}
				if len(available) == 1 {
					choice = available[0]
				}
			}
			selected, err := oci.ResolveStoredImage(ctx, backend, name, choice)
			if err != nil {
				return err
			}
			if _, exists := selectedByID[selected.StorageImageID]; !exists {
				selectedByID[selected.StorageImageID] = selected
			}
			if len(names) == 1 {
				solePlatform = selected.Platform
			}
			if _, err := digest.Parse(name); err == nil {
				requested[i] = selected.StorageImageID
			}
		}
		system := &types.SystemContext{}
		if explicit || len(names) == 1 {
			choice := platform
			if !explicit {
				choice = solePlatform
			}
			system.OSChoice = choice.OS
			system.ArchitectureChoice = choice.Architecture
			system.VariantChoice = choice.Variant
		}
		native, err := libimage.RuntimeFromStore(backend, &libimage.RuntimeOptions{SystemContext: system})
		if err != nil {
			return err
		}
		copied := imageCopyOptions(options, writer)
		copied.SourceLookupReferenceFunc = func(ref types.ImageReference) (types.ImageReference, error) {
			_, image, err := imagestorage.ResolveReference(ref)
			if err != nil {
				return nil, err
			}
			selected, exists := selectedByID[image.ID]
			if !exists {
				return nil, fmt.Errorf("unexpected native archive source image %s", image.ID)
			}
			// Reuse Coopr's manifest-bound native read reference. libimage remains
			// responsible for Docker format conversion, archive tags and serialization.
			return selectedStorageReference{ImageReference: ref, manifest: selected.Selected.Digest}, nil
		}
		saved := saveOptions
		copied.DirForceCompress = saved.DirForceCompress
		copied.OciAcceptUncompressedLayers = saved.OciAcceptUncompressedLayers
		saved.CopyOptions = copied
		return native.Save(ctx, requested, format, path, &saved)
	})
}

// ImageHistory selects an exact local platform and uses native layer history for
// sizes and parent IDs. Config history fields come from its verified config.
func ImageHistory(ctx context.Context, store StoreOptions, name string, platform v1.Platform, explicit bool) (history []libimage.ImageHistory, err error) {
	err = WithImageStore(ctx, store, func(backend storage.Store) error {
		if !explicit {
			available, err := oci.StoredImagePlatforms(ctx, backend, name)
			if err != nil {
				return err
			}
			if len(available) == 1 {
				platform = available[0]
			}
		}
		selected, err := oci.ResolveStoredImage(ctx, backend, name, platform)
		if err != nil {
			return err
		}
		native, err := libimage.RuntimeFromStore(backend, nil)
		if err != nil {
			return err
		}
		image, _, err := native.LookupImage(selected.StorageImageID, nil)
		if err != nil {
			return err
		}
		history, err = image.History(ctx)
		if err != nil {
			return err
		}
		var config v1.Image
		if err := json.Unmarshal(selected.ConfigData, &config); err != nil {
			return err
		}
		if len(history) != len(config.History) {
			return fmt.Errorf("native history differs from selected config for %s", platforms.Format(platform))
		}
		for i, entry := range config.History {
			target := &history[len(history)-1-i]
			target.Created = entry.Created
			target.CreatedBy = entry.CreatedBy
			target.Comment = entry.Comment
		}
		return nil
	})
	return history, err
}

// ImageExists checks only the selected local store. Missing names are not errors.
func ImageExists(ctx context.Context, store StoreOptions, name string) (exists bool, err error) {
	err = WithImageStore(ctx, store, func(backend storage.Store) error {
		_, err := oci.StoredImageName(backend, name)
		if errors.Is(err, storage.ErrImageUnknown) {
			return nil
		}
		if err != nil {
			return err
		}
		exists = true
		return nil
	})
	return exists, err
}

// loadArchiveInstance pins the source manifest digest, not just its platform.
// Native loader platform selection cannot distinguish two variants for one OS/arch.
func loadArchiveInstance(ctx context.Context, backend storage.Store, ref types.ImageReference, child v1.Descriptor, options libimage.CopyOptions) (_ string, retErr error) {
	system := &types.SystemContext{OSChoice: options.OS, ArchitectureChoice: options.Architecture, VariantChoice: options.Variant, SignaturePolicyPath: options.SignaturePolicyPath}
	if child.Platform != nil {
		system.OSChoice, system.ArchitectureChoice, system.VariantChoice = child.Platform.OS, child.Platform.Architecture, child.Platform.Variant
	}
	source, err := ref.NewImageSource(ctx, system)
	if err != nil {
		return "", err
	}
	defer func() { retErr = errors.Join(retErr, source.Close()) }()
	data, mediaType, err := source.GetManifest(ctx, &child.Digest)
	if err != nil {
		return "", err
	}
	if actual := oci.Descriptor(mediaType, data); actual.Digest != child.Digest || actual.Size != child.Size || actual.MediaType != child.MediaType {
		return "", errors.New("archive manifest differs from index descriptor")
	}
	var encoded v1.Manifest
	if err := json.Unmarshal(data, &encoded); err != nil {
		return "", err
	}
	if err := encoded.Config.Digest.Validate(); err != nil {
		return "", err
	}
	id := encoded.Config.Digest.Encoded()
	destination, err := imagestorage.Transport.NewStoreReference(backend, nil, id)
	if err != nil {
		return "", err
	}
	policy, err := signature.DefaultPolicy(system)
	if err != nil {
		return "", err
	}
	policyContext, err := signature.NewPolicyContext(policy)
	if err != nil {
		return "", err
	}
	defer func() { retErr = errors.Join(retErr, policyContext.Destroy()) }()
	copied, err := imagecopy.Image(ctx, policyContext, destination, selectedStorageReference{ImageReference: ref, manifest: child.Digest}, &imagecopy.Options{SourceCtx: system, DestinationCtx: system, PreserveDigests: true, ReportWriter: options.Writer})
	if err != nil {
		return "", err
	}
	if digest.FromBytes(copied) != child.Digest {
		return "", errors.New("loaded platform manifest differs from archived index")
	}
	return id, nil
}
