package buildah

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"coopr/internal/buildcontext"
	"coopr/internal/localstore"
	"coopr/internal/oci"
	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	upstream "go.podman.io/buildah"
	"go.podman.io/buildah/copier"
	"go.podman.io/image/v5/types"
	"go.podman.io/storage"
	storagearchive "go.podman.io/storage/pkg/archive"
	"go.podman.io/storage/pkg/idtools"
)

// PrimaryContext is one frozen filesystem context prepared before planning.
// SourceDateEpoch follows BuildKit's SOURCE_DATE_EPOCH=context semantics.
type PrimaryContext struct {
	Path            string
	SourceDateEpoch *int64
}

// MaterializePrimaryContext resolves a local, Git, or HTTP tar context to one
// directory which remains stable until cleanup is called.
func MaterializePrimaryContext(ctx context.Context, spec buildcontext.Spec, secrets, ssh []string) (PrimaryContext, func() error, error) {
	if ctx == nil {
		return PrimaryContext{}, nil, errors.New("primary context materialization context is nil")
	}
	if err := ctx.Err(); err != nil {
		return PrimaryContext{}, nil, err
	}
	switch spec.Kind {
	case buildcontext.Local:
		if spec.Path == "" {
			return PrimaryContext{}, nil, errors.New("primary local context has an empty path")
		}
		info, err := os.Stat(spec.Path)
		if err != nil {
			return PrimaryContext{}, nil, fmt.Errorf("inspect primary context: %w", err)
		}
		if info.Mode().IsRegular() {
			file, err := os.Open(spec.Path)
			if err != nil {
				return PrimaryContext{}, nil, fmt.Errorf("open context archive: %w", err)
			}
			defer func() { _ = file.Close() }()
			return MaterializeArchiveContext(ctx, file)
		}
		return PrimaryContext{Path: spec.Path}, func() error { return nil }, nil
	case buildcontext.Git:
		return materializePrimaryGitContext(ctx, spec, buildCredentialSource{secretSpecs: secrets, sshSpecs: ssh})
	case buildcontext.HTTPArchive:
		directory, epoch, cleanup, err := downloadHTTPContext(ctx, spec)
		if err != nil {
			return PrimaryContext{}, nil, err
		}
		return PrimaryContext{Path: directory, SourceDateEpoch: epoch}, cleanup, nil
	default:
		return PrimaryContext{}, nil, fmt.Errorf("primary context has unsupported kind %q", spec.Kind)
	}
}

// MaterializeArchiveContext freezes an uncompressed or compressed tar input
// using the same extraction and ownership rules as an HTTP build context.
func MaterializeArchiveContext(ctx context.Context, reader io.Reader) (PrimaryContext, func() error, error) {
	if ctx == nil || reader == nil {
		return PrimaryContext{}, nil, errors.New("archive context requires a context and input reader")
	}
	if err := ctx.Err(); err != nil {
		return PrimaryContext{}, nil, err
	}
	directory, err := os.MkdirTemp("", "coopr-primary-archive-")
	if err != nil {
		return PrimaryContext{}, nil, fmt.Errorf("create archive context directory: %w", err)
	}
	cleanup := func() error { return os.RemoveAll(directory) }
	if _, err := extractRemoteNamedContext(ctx, directory, reader); err != nil {
		return PrimaryContext{}, nil, errors.Join(fmt.Errorf("extract context archive: %w", err), cleanup())
	}
	return PrimaryContext{Path: directory}, cleanup, nil
}

func materializePrimaryGitContext(ctx context.Context, spec buildcontext.Spec, credentials buildCredentialSource) (PrimaryContext, func() error, error) {
	if spec.Reference == "" {
		return PrimaryContext{}, nil, errors.New("primary Git context has an empty URL")
	}
	options, err := normalizeGitNamedContextURL(spec.Reference)
	if err != nil {
		return PrimaryContext{}, nil, fmt.Errorf("primary Git context: %w", err)
	}
	temporary, err := os.MkdirTemp("", "coopr-primary-git-")
	if err != nil {
		return PrimaryContext{}, nil, fmt.Errorf("create primary Git context directory: %w", err)
	}
	cleanup := func() error { return os.RemoveAll(temporary) }
	repository := filepath.Join(temporary, "download")
	cloneOptions := gitCloneOptions{submodules: options.submodules, checksum: options.checksum, mtime: options.mtime}
	if options.fetchByCommit {
		cloneOptions.fetchCommit = options.checksum
	}
	contextDir, err := cloneGitSource(ctx, credentials, options.cloneSource, repository, cloneOptions)
	if err != nil {
		return PrimaryContext{}, nil, errors.Join(fmt.Errorf("clone primary Git context: %w", err), cleanup())
	}
	epoch, err := clonedGitCommitEpoch(ctx, repository)
	if err != nil {
		return PrimaryContext{}, nil, errors.Join(fmt.Errorf("resolve primary Git context commit time: %w", err), cleanup())
	}
	if !options.keepGitDir {
		if err := removeGitMetadata(repository); err != nil {
			return PrimaryContext{}, nil, errors.Join(fmt.Errorf("remove primary Git context metadata: %w", err), cleanup())
		}
	}
	return PrimaryContext{Path: contextDir, SourceDateEpoch: &epoch}, cleanup, nil
}

func clonedGitCommitEpoch(ctx context.Context, repository string) (int64, error) {
	executable, err := gitAddExecutable()
	if err != nil {
		return 0, err
	}
	environment := gitWorkerEnvironment(os.Environ())
	environment = removeEnvironment(environment, "HOME", "XDG_CONFIG_HOME", "SSH_AUTH_SOCK")
	environment = append(environment, "HOME=/dev/null", "XDG_CONFIG_HOME=/dev/null")
	commitTime, err := gitCommitTime(ctx, executable, environment, repository)
	if err != nil {
		return 0, err
	}
	return commitTime.Unix(), nil
}

// MaterializeNamedContext freezes a normalized named build context as one
// immutable image in the Buildah store. All context kinds therefore enter
// FROM, COPY --from, and RUN --mount through the same image-ID boundary.
func MaterializeNamedContext(ctx context.Context, spec buildcontext.Spec, platform v1.Platform, store storage.Store, system *types.SystemContext, resolver *oci.Resolver, secrets, ssh []string, artifacts ...string) (ResolvedImageSource, error) {
	if ctx == nil {
		return ResolvedImageSource{}, errors.New("named context materialization context is nil")
	}
	if err := ctx.Err(); err != nil {
		return ResolvedImageSource{}, err
	}
	if spec.Name == "" {
		return ResolvedImageSource{}, errors.New("named context name is empty")
	}
	if platform.OS != "linux" || platform.Architecture == "" {
		return ResolvedImageSource{}, errors.New("named context platform must specify Linux and architecture")
	}
	if store == nil {
		return ResolvedImageSource{}, errors.New("named context containers/storage store is nil")
	}

	switch spec.Kind {
	case buildcontext.Local:
		return materializeLocalNamedContext(ctx, spec, platform, store, system, artifacts)
	case buildcontext.DockerImage:
		if spec.Reference == "" {
			return ResolvedImageSource{}, fmt.Errorf("named context %q has an empty image reference", spec.Name)
		}
		return ResolveImageSource(ctx, resolver, spec.Reference, platform, store, system)
	case buildcontext.OCILayout:
		return materializeLayoutNamedContext(ctx, spec, platform, store, system)
	case buildcontext.Git:
		return materializeGitNamedContext(ctx, spec, platform, store, system, buildCredentialSource{secretSpecs: secrets, sshSpecs: ssh})
	case buildcontext.HTTPArchive:
		return materializeHTTPNamedContext(ctx, spec, platform, store, system)
	default:
		return ResolvedImageSource{}, fmt.Errorf("named context %q has unsupported kind %q", spec.Name, spec.Kind)
	}
}

func materializeGitNamedContext(ctx context.Context, spec buildcontext.Spec, platform v1.Platform, store storage.Store, system *types.SystemContext, credentials buildCredentialSource) (_ ResolvedImageSource, retErr error) {
	if spec.Reference == "" {
		return ResolvedImageSource{}, fmt.Errorf("git named context %q has an empty URL", spec.Name)
	}
	options, err := normalizeGitNamedContextURL(spec.Reference)
	if err != nil {
		return ResolvedImageSource{}, fmt.Errorf("git named context %q: %w", spec.Name, err)
	}
	temporary, err := os.MkdirTemp("", "coopr-named-git-")
	if err != nil {
		return ResolvedImageSource{}, fmt.Errorf("create Git named context %q directory: %w", spec.Name, err)
	}
	defer func() { retErr = errors.Join(retErr, os.RemoveAll(temporary)) }()
	repository := filepath.Join(temporary, "download")
	cloneOptions := gitCloneOptions{submodules: options.submodules, checksum: options.checksum, mtime: options.mtime}
	if options.fetchByCommit {
		cloneOptions.fetchCommit = options.checksum
	}
	contextDir, err := cloneGitSource(ctx, credentials, options.cloneSource, repository, cloneOptions)
	if err != nil {
		return ResolvedImageSource{}, fmt.Errorf("clone Git named context %q: %w", spec.Name, err)
	}
	if !options.keepGitDir {
		if err := removeGitMetadata(repository); err != nil {
			return ResolvedImageSource{}, fmt.Errorf("remove Git metadata from named context %q: %w", spec.Name, err)
		}
	}
	return materializeLocalNamedContext(ctx, buildcontext.Spec{
		Name: spec.Name, Kind: buildcontext.Local, Path: contextDir,
	}, platform, store, system, nil)
}

func removeGitMetadata(root string) error {
	return filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Name() != ".git" {
			return nil
		}
		if err := os.RemoveAll(path); err != nil {
			return err
		}
		if entry.IsDir() {
			return filepath.SkipDir
		}
		return nil
	})
}

type gitNamedContextOptions struct {
	cloneSource   string
	checksum      string
	submodules    bool
	keepGitDir    bool
	mtime         string
	fetchByCommit bool
}

func normalizeGitNamedContextURL(source string) (gitNamedContextOptions, error) {
	options := gitNamedContextOptions{submodules: true, mtime: "checkout"}
	base, fragment, hasFragment := strings.Cut(source, "#")
	if hasFragment && strings.ContainsAny(fragment, "#?") {
		return gitNamedContextOptions{}, errors.New("git context fragment cannot contain '#' or '?'")
	}
	remote, rawQuery, hasQuery := strings.Cut(base, "?")
	if !hasQuery {
		if !hasFragment {
			options.cloneSource = source + "#HEAD"
			return options, nil
		}
		options.cloneSource = source
		return options, nil
	}
	query, err := url.ParseQuery(rawQuery)
	if err != nil {
		return gitNamedContextOptions{}, fmt.Errorf("parse Git context query: %w", err)
	}
	allowed := map[string]bool{"branch": true, "tag": true, "ref": true, "subdir": true, "checksum": true, "commit": true, "submodules": true, "keep-git-dir": true, "mtime": true, "fetch-by-commit": true}
	for key := range query {
		if !allowed[key] {
			return gitNamedContextOptions{}, fmt.Errorf("unsupported Git context query %q", key)
		}
		if len(query[key]) != 1 {
			return gitNamedContextOptions{}, fmt.Errorf("git context query %q must be specified once", key)
		}
		if !isGitContextBooleanQuery(key) && query[key][0] == "" {
			return gitNamedContextOptions{}, fmt.Errorf("git context query %q requires a value", key)
		}
	}
	options.submodules, err = gitContextQueryBool(query, "submodules", true)
	if err != nil {
		return gitNamedContextOptions{}, err
	}
	options.keepGitDir, err = gitContextQueryBool(query, "keep-git-dir", false)
	if err != nil {
		return gitNamedContextOptions{}, err
	}
	options.fetchByCommit, err = gitContextQueryBool(query, "fetch-by-commit", false)
	if err != nil {
		return gitNamedContextOptions{}, err
	}
	options.mtime = query.Get("mtime")
	if options.mtime == "" {
		options.mtime = "checkout"
	} else if options.mtime != "checkout" && options.mtime != "commit" {
		return gitNamedContextOptions{}, errors.New("git context query \"mtime\" must be checkout or commit")
	}
	queryRef := query.Get("ref")
	tag := query.Get("tag")
	if tag != "" {
		tag = qualifyGitContextRef(tag, "refs/tags/")
		if queryRef != "" && queryRef != tag {
			return gitNamedContextOptions{}, errors.New("git context ref and tag selectors disagree")
		}
		queryRef = tag
	}
	branch := query.Get("branch")
	if branch != "" {
		if tag != "" {
			return gitNamedContextOptions{}, errors.New("git context branch conflicts with tag")
		}
		branch = qualifyGitContextRef(branch, "refs/heads/")
		if queryRef != "" && queryRef != branch {
			return gitNamedContextOptions{}, errors.New("git context ref and branch selectors disagree")
		}
		queryRef = branch
	}
	fragmentRef, fragmentSubdir := "", ""
	if hasFragment {
		fragmentRef, fragmentSubdir, _ = strings.Cut(fragment, ":")
		if fragmentRef == "" {
			fragmentRef = "HEAD"
		}
	}
	if queryRef != "" && fragmentRef != "" && fragmentRef != queryRef {
		return gitNamedContextOptions{}, errors.New("git context fragment and query ref selectors disagree")
	}
	ref := queryRef
	if ref == "" {
		ref = fragmentRef
		if ref == "" {
			ref = "HEAD"
		}
	}
	subdir := query.Get("subdir")
	if subdir != "" && fragmentSubdir != "" && subdir != fragmentSubdir {
		return gitNamedContextOptions{}, errors.New("git context fragment and query subdir selectors disagree")
	}
	if subdir == "" {
		subdir = fragmentSubdir
	}
	if strings.ContainsAny(ref, "#:") {
		return gitNamedContextOptions{}, errors.New("git context ref cannot contain '#' or ':' with embedded Buildah")
	}
	options.checksum = query.Get("checksum")
	if commit := query.Get("commit"); commit != "" {
		if options.checksum != "" && options.checksum != commit {
			return gitNamedContextOptions{}, errors.New("git context checksum and commit selectors disagree")
		}
		options.checksum = commit
	}
	if options.fetchByCommit && !isFullGitCommitSHA(options.checksum) {
		return gitNamedContextOptions{}, errors.New("git context fetch-by-commit requires a full lowercase commit SHA checksum")
	}
	options.cloneSource = remote + "#" + ref
	if subdir != "" {
		options.cloneSource += ":" + subdir
	}
	return options, nil
}

func isGitContextBooleanQuery(key string) bool {
	switch key {
	case "submodules", "keep-git-dir", "fetch-by-commit":
		return true
	default:
		return false
	}
}

func qualifyGitContextRef(value, prefix string) string {
	if strings.HasPrefix(value, prefix) {
		return value
	}
	return prefix + value
}

func isFullGitCommitSHA(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	return strings.IndexFunc(value, func(character rune) bool {
		return character < '0' || character > '9' && (character < 'a' || character > 'f')
	}) == -1
}

func gitContextQueryBool(query url.Values, key string, defaultValue bool) (bool, error) {
	values, found := query[key]
	if !found {
		return defaultValue, nil
	}
	if len(values) != 1 {
		return false, fmt.Errorf("git context query %q must be specified once", key)
	}
	if values[0] == "" {
		return true, nil
	}
	value, err := strconv.ParseBool(values[0])
	if err != nil {
		return false, fmt.Errorf("git context query %q must be a boolean: %w", key, err)
	}
	return value, nil
}

func materializeHTTPNamedContext(ctx context.Context, spec buildcontext.Spec, platform v1.Platform, store storage.Store, system *types.SystemContext) (_ ResolvedImageSource, retErr error) {
	directory, _, cleanup, err := downloadHTTPContext(ctx, spec)
	if err != nil {
		return ResolvedImageSource{}, err
	}
	defer func() { retErr = errors.Join(retErr, cleanup()) }()
	return materializeLocalNamedContext(ctx, buildcontext.Spec{
		Name: spec.Name, Kind: buildcontext.Local, Path: directory,
	}, platform, store, system, nil)
}

func downloadHTTPContext(ctx context.Context, spec buildcontext.Spec) (_ string, _ *int64, cleanup func() error, retErr error) {
	if spec.Reference == "" {
		return "", nil, nil, fmt.Errorf("HTTP context %q has an empty URL", spec.Name)
	}
	parsed, err := url.Parse(spec.Reference)
	if err != nil {
		return "", nil, nil, fmt.Errorf("parse HTTP context %q URL: %w", spec.Name, err)
	}
	if parsed.User != nil {
		return "", nil, nil, fmt.Errorf("HTTP context %q URL must not contain credentials", spec.Name)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, spec.Reference, nil)
	if err != nil {
		return "", nil, nil, fmt.Errorf("create HTTP context %q request: %w", spec.Name, err)
	}
	client := &http.Client{CheckRedirect: func(request *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("remote context exceeded 10 HTTP redirects")
		}
		if request.URL.User != nil {
			return errors.New("remote context redirect must not contain credentials")
		}
		request.Header.Del("Authorization")
		request.Header.Del("Referer")
		return nil
	}}
	response, err := client.Do(request) //nolint:gosec // The authored build context is intentionally remote.
	if err != nil {
		return "", nil, nil, fmt.Errorf("download HTTP context %q: %w", spec.Name, err)
	}
	defer func() { retErr = errors.Join(retErr, response.Body.Close()) }()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return "", nil, nil, fmt.Errorf("download HTTP context %q: HTTP status %s", spec.Name, response.Status)
	}
	directory, err := os.MkdirTemp("", "coopr-primary-http-")
	if err != nil {
		return "", nil, nil, fmt.Errorf("create HTTP context directory: %w", err)
	}
	cleanup = func() error { return os.RemoveAll(directory) }
	archiveEpoch, err := extractRemoteNamedContext(ctx, directory, response.Body)
	if err != nil {
		return "", nil, nil, errors.Join(fmt.Errorf("extract HTTP context %q as a tar archive: %w", spec.Name, err), cleanup())
	}
	epoch := archiveEpoch
	if modified := response.Header.Get("Last-Modified"); modified != "" {
		if parsed, parseErr := http.ParseTime(modified); parseErr == nil {
			epoch = parsed
		}
	}
	var unix *int64
	if !epoch.IsZero() {
		value := epoch.Unix()
		unix = &value
	}
	return directory, unix, cleanup, nil
}

func extractRemoteNamedContext(ctx context.Context, directory string, archiveReader io.Reader) (_ time.Time, retErr error) {
	if ctx == nil {
		return time.Time{}, errors.New("remote context extraction context is nil")
	}
	stream, err := storagearchive.DecompressStream(&contextReader{ctx: ctx, reader: archiveReader})
	if err != nil {
		return time.Time{}, fmt.Errorf("decompress archive: %w", err)
	}
	defer func() { retErr = errors.Join(retErr, stream.Close()) }()
	archive, err := os.CreateTemp("", "coopr-named-context-*.tar")
	if err != nil {
		return time.Time{}, fmt.Errorf("stage remote context archive: %w", err)
	}
	defer func() {
		retErr = errors.Join(retErr, archive.Close(), os.Remove(archive.Name()))
	}()
	reader := tar.NewReader(io.TeeReader(stream, archive))
	var newestRegular time.Time
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return time.Time{}, fmt.Errorf("read remote context archive: %w", err)
		}
		if !filepath.IsLocal(filepath.FromSlash(header.Name)) {
			return time.Time{}, fmt.Errorf("remote context archive contains non-local path %q", header.Name)
		}
		if header.Typeflag == tar.TypeLink && !filepath.IsLocal(filepath.FromSlash(header.Linkname)) {
			return time.Time{}, fmt.Errorf("remote context archive contains non-local hard-link target %q", header.Linkname)
		}
		// Tar also encodes regular files with a zero type flag.
		if header.Typeflag == tar.TypeReg || header.Typeflag == 0 {
			if header.ModTime.After(newestRegular) {
				newestRegular = header.ModTime
			}
		}
	}
	if _, err := io.Copy(archive, &contextReader{ctx: ctx, reader: stream}); err != nil {
		return time.Time{}, fmt.Errorf("finish remote context archive: %w", err)
	}
	if _, err := archive.Seek(0, io.SeekStart); err != nil {
		return time.Time{}, fmt.Errorf("rewind remote context archive: %w", err)
	}
	owner := idtools.IDPair{UID: os.Getuid(), GID: os.Getgid()}
	if err := copier.Put(directory, directory, copier.PutOptions{
		DefaultDirOwner: &owner, ChownDirs: &owner, ChownFiles: &owner,
		StripSetuidBit: true, StripSetgidBit: true, IgnoreDevices: true,
	}, &contextReader{ctx: ctx, reader: archive}); err != nil {
		return time.Time{}, err
	}
	return newestRegular, nil
}

func materializeLocalNamedContext(ctx context.Context, spec buildcontext.Spec, platform v1.Platform, store storage.Store, system *types.SystemContext, artifacts []string) (_ ResolvedImageSource, retErr error) {
	if spec.Path == "" {
		return ResolvedImageSource{}, fmt.Errorf("local named context %q has an empty path", spec.Name)
	}
	policy, err := prepareContextPolicy(spec.Path, artifacts)
	if err != nil {
		return ResolvedImageSource{}, fmt.Errorf("prepare local named context %q: %w", spec.Name, err)
	}
	options, err := policy.apply(upstream.AddAndCopyOptions{})
	if err != nil {
		return ResolvedImageSource{}, fmt.Errorf("prepare local named context %q: %w", spec.Name, err)
	}
	snapshot, cleanupSnapshot, err := snapshotContext(options.ContextDir, options.Excludes, nil, nil)
	if err != nil {
		return ResolvedImageSource{}, fmt.Errorf("snapshot local named context %q: %w", spec.Name, err)
	}
	defer func() { retErr = errors.Join(retErr, cleanupSnapshot()) }()

	temporaryRoot, err := os.MkdirTemp("", "coopr-named-context-")
	if err != nil {
		return ResolvedImageSource{}, fmt.Errorf("create local named context staging directory: %w", err)
	}
	defer func() { retErr = errors.Join(retErr, os.RemoveAll(temporaryRoot)) }()
	tarPath := filepath.Join(temporaryRoot, "rootfs.tar")
	tarFile, err := os.OpenFile(tarPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return ResolvedImageSource{}, fmt.Errorf("create local named context snapshot: %w", err)
	}
	digester := digest.Canonical.Digester()
	copyErr := copier.Get(snapshot, snapshot, copier.GetOptions{}, []string{"."}, &contextWriter{ctx: ctx, writer: tarFile, hash: digester.Hash()})
	closeErr := tarFile.Close()
	if err := errors.Join(copyErr, closeErr); err != nil {
		return ResolvedImageSource{}, fmt.Errorf("archive local named context %q: %w", spec.Name, err)
	}
	info, err := os.Stat(tarPath)
	if err != nil {
		return ResolvedImageSource{}, fmt.Errorf("inspect local named context snapshot: %w", err)
	}
	layer := v1.Descriptor{MediaType: oci.ComponentPackageType, Digest: digester.Digest(), Size: info.Size()}
	configData, err := json.Marshal(v1.Image{
		Platform: platform,
		RootFS:   v1.RootFS{Type: "layers", DiffIDs: []digest.Digest{layer.Digest}},
	})
	if err != nil {
		return ResolvedImageSource{}, fmt.Errorf("encode local named context config: %w", err)
	}
	imageID, rawConfig, err := ImportPackageSnapshot(ctx, store, system, oci.Package{
		Stage: spec.Name, Descriptor: layer, Config: configData,
	}, tarPath, platform)
	if err != nil {
		return ResolvedImageSource{}, fmt.Errorf("import local named context %q: %w", spec.Name, err)
	}
	selected, err := localContextManifest(rawConfig, layer)
	if err != nil {
		return ResolvedImageSource{}, err
	}
	return ResolvedImageSource{
		ImageID: imageID, Root: selected, Selected: selected, ConfigData: rawConfig,
		Reference: "local-context:" + spec.Name,
	}, nil
}

func materializeLayoutNamedContext(ctx context.Context, spec buildcontext.Spec, platform v1.Platform, store storage.Store, system *types.SystemContext) (_ ResolvedImageSource, retErr error) {
	if spec.Path == "" || spec.Reference == "" {
		return ResolvedImageSource{}, fmt.Errorf("OCI-layout named context %q requires a path and selector", spec.Name) //nolint:staticcheck // OCI is the specification name.
	}
	resolved, err := oci.ResolveLayoutImage(ctx, spec.Path, spec.Reference, platform)
	if err != nil {
		return ResolvedImageSource{}, fmt.Errorf("resolve named context %q: %w", spec.Name, err)
	}
	staging, err := os.MkdirTemp("", "coopr-named-layout-")
	if err != nil {
		return ResolvedImageSource{}, fmt.Errorf("create named context layout staging directory: %w", err)
	}
	defer func() { retErr = errors.Join(retErr, os.RemoveAll(staging)) }()
	if err := localstore.CopySelectedImage(ctx, staging, resolved.Source(), resolved.Root, resolved.Selected); err != nil {
		return ResolvedImageSource{}, fmt.Errorf("freeze named context %q: %w", spec.Name, err)
	}
	imageID, err := ImportSelectedImage(ctx, store, system, staging, resolved.Selected)
	if err != nil {
		return ResolvedImageSource{}, fmt.Errorf("import named context %q: %w", spec.Name, err)
	}
	return ResolvedImageSource{
		ImageID: imageID, Root: resolved.Root, Selected: resolved.Selected,
		ConfigData: append(json.RawMessage(nil), resolved.ConfigData...), Reference: resolved.Reference,
	}, nil
}

func localContextManifest(configData json.RawMessage, layer v1.Descriptor) (v1.Descriptor, error) {
	config := oci.Descriptor(v1.MediaTypeImageConfig, configData)
	layer.MediaType = v1.MediaTypeImageLayer
	manifestData, err := json.Marshal(v1.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: v1.MediaTypeImageManifest,
		Config:    config, Layers: []v1.Descriptor{layer},
	})
	if err != nil {
		return v1.Descriptor{}, fmt.Errorf("encode local named context manifest: %w", err)
	}
	return oci.Descriptor(v1.MediaTypeImageManifest, manifestData), nil
}

// contextWriter keeps context cancellation and hashing coupled to the bytes
// accepted by the snapshot file.
type contextWriter struct {
	ctx    context.Context
	writer *os.File
	hash   interface{ Write([]byte) (int, error) }
}

func (writer *contextWriter) Write(data []byte) (int, error) {
	if err := writer.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := writer.writer.Write(data)
	if n > 0 {
		if _, hashErr := writer.hash.Write(data[:n]); hashErr != nil && err == nil {
			err = hashErr
		}
	}
	return n, err
}
