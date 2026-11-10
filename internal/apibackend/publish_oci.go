package apibackend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/coopr/api"
	"golang.org/x/sys/unix"
)

// publishOCISource selects an explicitly pinned, single-image layout. It does not
// import into native storage, select a host platform, or reconstruct an image.
func publishOCISource(ctx context.Context, p *api.Publish) (string, v1.Descriptor, error) {
	if err := ctx.Err(); err != nil {
		return "", v1.Descriptor{}, err
	}
	if p.Artifact != "image" {
		return "", v1.Descriptor{}, errors.New("OCI layout publication requires artifact image")
	}
	wanted, err := digest.Parse(p.SourceDigest)
	if err != nil || wanted.Algorithm() != digest.SHA256 {
		return "", v1.Descriptor{}, errors.New("OCI layout publication requires a valid sha256 sourceDigest")
	}
	path := strings.TrimPrefix(p.Source, "oci:")
	if err := absolute(path); err != nil {
		return "", v1.Descriptor{}, err
	}
	path, err = canonical(path)
	if err != nil {
		return "", v1.Descriptor{}, err
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return "", v1.Descriptor{}, fmt.Errorf("open OCI publication source: %w", err)
	}
	defer func() { _ = root.Close() }()
	read := func(name string) ([]byte, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		f, err := root.OpenFile(name, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
		if err != nil {
			return nil, err
		}
		defer func() { _ = f.Close() }()
		info, err := f.Stat()
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() || info.Size() > api.MaxEventBytes {
			return nil, errors.New("OCI publication metadata must be a bounded regular file")
		}
		data, err := io.ReadAll(io.LimitReader(f, api.MaxEventBytes+1))
		if err != nil {
			return nil, err
		}
		if len(data) > api.MaxEventBytes {
			return nil, errors.New("OCI publication metadata exceeds limit")
		}
		return data, ctx.Err()
	}
	data, err := read(v1.ImageLayoutFile)
	if err != nil {
		return "", v1.Descriptor{}, err
	}
	var layout v1.ImageLayout
	if err := json.Unmarshal(data, &layout); err != nil || layout.Version != v1.ImageLayoutVersion {
		return "", v1.Descriptor{}, errors.New("invalid OCI publication layout version")
	}
	data, err = read(v1.ImageIndexFile)
	if err != nil {
		return "", v1.Descriptor{}, err
	}
	var index v1.Index
	if err := json.Unmarshal(data, &index); err != nil || index.SchemaVersion != 2 || len(index.Manifests) != 1 {
		return "", v1.Descriptor{}, errors.New("OCI publication layout must select exactly one image manifest")
	}
	selected := index.Manifests[0]
	if selected.Digest != wanted || selected.Size < 0 || selected.Size > api.MaxEventBytes {
		return "", v1.Descriptor{}, errors.New("OCI publication root differs from sourceDigest or exceeds metadata limit")
	}
	if selected.MediaType != v1.MediaTypeImageManifest && selected.MediaType != "application/vnd.docker.distribution.manifest.v2+json" {
		return "", v1.Descriptor{}, errors.New("OCI publication source must select an image manifest, not an index or component")
	}
	data, err = read(filepath.Join(v1.ImageBlobsDir, "sha256", wanted.Encoded()))
	if err != nil {
		return "", v1.Descriptor{}, err
	}
	if int64(len(data)) != selected.Size || digest.FromBytes(data) != wanted {
		return "", v1.Descriptor{}, errors.New("OCI publication manifest does not match sourceDigest")
	}
	var manifest v1.Manifest
	if err := json.Unmarshal(data, &manifest); err != nil || manifest.SchemaVersion != 2 || manifest.ArtifactType != "" || (manifest.Config.MediaType != v1.MediaTypeImageConfig && manifest.Config.MediaType != "application/vnd.docker.container.image.v1+json") {
		return "", v1.Descriptor{}, errors.New("OCI publication source is not a runnable image manifest")
	}
	return path, selected, nil
}
