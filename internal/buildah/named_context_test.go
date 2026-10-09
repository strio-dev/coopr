package buildah

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"coopr/internal/buildcontext"
	"coopr/internal/oci"
	"coopr/internal/planner"
	"github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	upstream "go.podman.io/buildah"
	"go.podman.io/buildah/define"
	"go.podman.io/image/v5/types"
	"go.podman.io/storage"
	"oras.land/oras-go/v2/content"
	orasoci "oras.land/oras-go/v2/content/oci"
)

func TestMaterializeNamedContextRejectsInvalidRequests(t *testing.T) {
	platform := v1.Platform{OS: "linux", Architecture: runtime.GOARCH}
	if _, err := MaterializeNamedContext(nil, buildcontext.Spec{Name: "src", Kind: buildcontext.Local, Path: t.TempDir()}, platform, nil, nil, nil, nil, nil); err == nil { //nolint:staticcheck // Verifies explicit nil rejection.
		t.Fatal("MaterializeNamedContext accepted a nil context")
	}
	if _, err := MaterializeNamedContext(context.Background(), buildcontext.Spec{Name: "src", Kind: buildcontext.Local, Path: t.TempDir()}, platform, nil, nil, nil, nil, nil); err == nil {
		t.Fatal("MaterializeNamedContext accepted a nil store")
	}
}

func TestMaterializePrimaryHTTPContextTimestamp(t *testing.T) {
	archive := &bytes.Buffer{}
	tarWriter := tar.NewWriter(archive)
	for _, file := range []struct {
		name string
		when time.Time
	}{{"older", time.Unix(100, 0)}, {"newer", time.Unix(200, 0)}} {
		if err := tarWriter.WriteHeader(&tar.Header{Name: file.name, Mode: 0o644, Size: 1, ModTime: file.when}); err != nil {
			t.Fatal(err)
		}
		if _, err := tarWriter.Write([]byte("x")); err != nil {
			t.Fatal(err)
		}
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, modified string
		want           int64
	}{{name: "archive mtime", want: 200}, {name: "Last-Modified", modified: time.Unix(300, 0).UTC().Format(http.TimeFormat), want: 300}} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				if test.modified != "" {
					writer.Header().Set("Last-Modified", test.modified)
				}
				_, _ = writer.Write(archive.Bytes())
			}))
			defer server.Close()
			primary, cleanup, err := MaterializePrimaryContext(context.Background(), buildcontext.Spec{
				Name: "context", Kind: buildcontext.HTTPArchive, Reference: server.URL + "/context.tar",
			}, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = cleanup() }()
			if primary.SourceDateEpoch == nil || *primary.SourceDateEpoch != test.want {
				t.Fatalf("SOURCE_DATE_EPOCH = %v, want %d", primary.SourceDateEpoch, test.want)
			}
			if data, err := os.ReadFile(filepath.Join(primary.Path, "newer")); err != nil || string(data) != "x" {
				t.Fatalf("materialized file = %q, %v", data, err)
			}
		})
	}
}

func TestMaterializePrimaryContextHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, cleanup, err := MaterializePrimaryContext(ctx, buildcontext.Spec{
		Name: "context", Kind: buildcontext.HTTPArchive, Reference: "https://example.invalid/context.tar",
	}, nil, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled materialization error = %v", err)
	}
	if cleanup != nil {
		t.Fatal("canceled materialization returned cleanup for an uncreated context")
	}
}

func TestMaterializePrimaryGitContextTimestamp(t *testing.T) {
	source, commit := gitHTTPSubmoduleFixture(t, "")
	primary, cleanup, err := MaterializePrimaryContext(context.Background(), buildcontext.Spec{
		Name: "context", Kind: buildcontext.Git, Reference: source + "?ref=" + commit + "&checksum=" + commit,
	}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cleanup() }()
	if primary.SourceDateEpoch == nil || *primary.SourceDateEpoch != 946684800 {
		t.Fatalf("SOURCE_DATE_EPOCH = %v, want 946684800", primary.SourceDateEpoch)
	}
	if data, err := os.ReadFile(filepath.Join(primary.Path, "proof")); err != nil || string(data) != "parent repository\n" {
		t.Fatalf("materialized file = %q, %v", data, err)
	}
	if _, err := os.Lstat(filepath.Join(primary.Path, ".git")); !os.IsNotExist(err) {
		t.Fatalf("primary context retained Git metadata: %v", err)
	}
}

func TestNormalizeGitNamedContextURL(t *testing.T) {
	for _, test := range []struct {
		name, source, clone, checksum, message string
		submodules, keepGitDir                 bool
		mtime                                  string
		fetchByCommit                          bool
	}{
		{name: "default", source: "https://example.com/source.git", clone: "https://example.com/source.git#HEAD", submodules: true},
		{name: "legacy fragment", source: "git://example.com/source.git#main:src", clone: "git://example.com/source.git#main:src", submodules: true},
		{name: "branch query", source: "https://example.com/source.git?branch=main&subdir=src&checksum=abcdef", clone: "https://example.com/source.git#refs/heads/main:src", checksum: "abcdef", submodules: true},
		{name: "colon in query subdir", source: "https://example.com/source.git?subdir=src%3Agenerated", clone: "https://example.com/source.git#HEAD:src:generated", submodules: true},
		{name: "hash in query subdir", source: "https://example.com/source.git?subdir=src%23generated", clone: "https://example.com/source.git#HEAD:src#generated", submodules: true},
		{name: "qualified branch query", source: "https://example.com/source.git?branch=refs/heads/main", clone: "https://example.com/source.git#refs/heads/main", submodules: true},
		{name: "tag query", source: "https://example.com/source.git?tag=main", clone: "https://example.com/source.git#refs/tags/main", submodules: true},
		{name: "qualified tag query", source: "https://example.com/source.git?tag=refs/tags/main", clone: "https://example.com/source.git#refs/tags/main", submodules: true},
		{name: "consistent ref and branch query", source: "https://example.com/source.git?ref=refs/heads/main&branch=main", clone: "https://example.com/source.git#refs/heads/main", submodules: true},
		{name: "consistent ref and tag query", source: "https://example.com/source.git?ref=refs/tags/v1&tag=v1", clone: "https://example.com/source.git#refs/tags/v1", submodules: true},
		{name: "SSH query", source: "ssh://git@example.com/source.git?branch=main", clone: "ssh://git@example.com/source.git#refs/heads/main", submodules: true},
		{name: "SCP query", source: "git@example.com:team/source.git?tag=v1", clone: "git@example.com:team/source.git#refs/tags/v1", submodules: true},
		{name: "commit alias", source: "https://example.com/source.git?ref=pull/42/head&commit=012345", clone: "https://example.com/source.git#pull/42/head", checksum: "012345", submodules: true},
		{name: "controls", source: "https://example.com/source.git?submodules=false&keep-git-dir=true", clone: "https://example.com/source.git#HEAD", keepGitDir: true},
		{name: "conflicting branch and tag", source: "https://example.com/source.git?branch=main&tag=v1", message: "branch conflicts with tag"},
		{name: "conflicting ref and branch", source: "https://example.com/source.git?ref=refs/heads/other&branch=main", message: "ref and branch selectors disagree"},
		{name: "conflicting ref and tag", source: "https://example.com/source.git?ref=refs/tags/other&tag=v1", message: "ref and tag selectors disagree"},
		{name: "unknown query", source: "https://example.com/source.git?depth=2", message: "unsupported"},
		{name: "fragment with control query", source: "https://example.com/source.git?submodules=false#main:src", clone: "https://example.com/source.git#main:src"},
		{name: "consistent fragment and query selectors", source: "https://example.com/source.git?branch=main&subdir=src#refs/heads/main:src", clone: "https://example.com/source.git#refs/heads/main:src", submodules: true},
		{name: "normalized branch conflicts with short fragment", source: "https://example.com/source.git?branch=main#main", message: "ref selectors disagree"},
		{name: "conflicting fragment ref", source: "https://example.com/source.git?branch=main#other", message: "ref selectors disagree"},
		{name: "conflicting fragment subdir", source: "https://example.com/source.git?subdir=src#main:other", message: "subdir selectors disagree"},
		{name: "valueless submodules", source: "https://example.com/source.git?submodules", clone: "https://example.com/source.git#HEAD", submodules: true},
		{name: "numeric false submodules", source: "https://example.com/source.git?submodules=0", clone: "https://example.com/source.git#HEAD"},
		{name: "invalid submodules", source: "https://example.com/source.git?submodules=recursive", message: "must be a boolean"},
		{name: "valueless keep Git", source: "https://example.com/source.git?keep-git-dir", clone: "https://example.com/source.git#HEAD", submodules: true, keepGitDir: true},
		{name: "numeric true keep Git", source: "https://example.com/source.git?keep-git-dir=1", clone: "https://example.com/source.git#HEAD", submodules: true, keepGitDir: true},
		{name: "invalid keep Git", source: "https://example.com/source.git?keep-git-dir=sometimes", message: "must be a boolean"},
		{name: "commit mtime", source: "https://example.com/source.git?mtime=commit", clone: "https://example.com/source.git#HEAD", submodules: true, mtime: "commit"},
		{name: "invalid mtime", source: "https://example.com/source.git?mtime=now", message: "checkout or commit"},
		{name: "fetch by commit", source: "https://example.com/source.git?ref=refs/heads/moved&checksum=0123456789abcdef0123456789abcdef01234567&fetch-by-commit", clone: "https://example.com/source.git#refs/heads/moved", checksum: "0123456789abcdef0123456789abcdef01234567", submodules: true, fetchByCommit: true},
		{name: "disabled fetch by commit", source: "https://example.com/source.git?fetch-by-commit=false", clone: "https://example.com/source.git#HEAD", submodules: true},
		{name: "short fetch checksum", source: "https://example.com/source.git?checksum=0123456789ab&fetch-by-commit=true", message: "full lowercase"},
		{name: "uppercase fetch checksum", source: "https://example.com/source.git?checksum=0123456789ABCDEF0123456789ABCDEF01234567&fetch-by-commit=true", message: "full lowercase"},
		{name: "valueless branch", source: "https://example.com/source.git?branch", message: "requires a value"},
		{name: "valueless tag", source: "https://example.com/source.git?tag", message: "requires a value"},
		{name: "valueless ref", source: "https://example.com/source.git?ref", message: "requires a value"},
		{name: "valueless subdir", source: "https://example.com/source.git?subdir", message: "requires a value"},
		{name: "valueless checksum", source: "https://example.com/source.git?checksum", message: "requires a value"},
		{name: "valueless commit", source: "https://example.com/source.git?commit", message: "requires a value"},
		{name: "valueless mtime", source: "https://example.com/source.git?mtime", message: "requires a value"},
	} {
		t.Run(test.name, func(t *testing.T) {
			options, err := normalizeGitNamedContextURL(test.source)
			if test.message != "" {
				if err == nil || !strings.Contains(err.Error(), test.message) {
					t.Fatalf("normalize error = %v, want %q", err, test.message)
				}
				return
			}
			if err != nil || options.cloneSource != test.clone || options.checksum != test.checksum || options.submodules != test.submodules || options.keepGitDir != test.keepGitDir ||
				test.mtime != "" && options.mtime != test.mtime || options.fetchByCommit != test.fetchByCommit {
				t.Fatalf("normalize = %+v, %v; want clone=%q checksum=%q submodules=%t keepGitDir=%t", options, err, test.clone, test.checksum, test.submodules, test.keepGitDir)
			}
		})
	}
}

func TestGitQuerySubdirectoryPreservesSeparatorsThroughCloneSource(t *testing.T) {
	for _, test := range []struct {
		encoded string
		want    string
	}{
		{encoded: "src%3Agenerated", want: "src:generated"},
		{encoded: "src%23generated", want: "src#generated"},
	} {
		options, err := normalizeGitNamedContextURL("https://example.com/source.git?subdir=" + test.encoded)
		if err != nil {
			t.Fatal(err)
		}
		_, subdir, _, err := parseGitAddSource(options.cloneSource)
		if err != nil || subdir != test.want {
			t.Fatalf("roundtrip %q = %q, %v; want %q", test.encoded, subdir, err, test.want)
		}
	}
}

func TestMaterializeSSHGitNamedContextRequiresExplicitCredentials(t *testing.T) {
	platform := v1.Platform{OS: "linux", Architecture: runtime.GOARCH}
	_, err := materializeGitNamedContext(context.Background(), buildcontext.Spec{
		Name: "private", Kind: buildcontext.Git, Reference: "git@example.invalid:team/repository.git",
	}, platform, nil, nil, buildCredentialSource{})
	if err == nil || !strings.Contains(err.Error(), "requires --ssh default") {
		t.Fatalf("missing SSH source error = %v", err)
	}

	privateKey := filepath.Join(t.TempDir(), "id_ed25519")
	runCommand(t, "ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", privateKey)
	_, err = materializeGitNamedContext(context.Background(), buildcontext.Spec{
		Name: "private", Kind: buildcontext.Git, Reference: "ssh://git@example.invalid/team/repository.git",
	}, platform, nil, nil, buildCredentialSource{sshSpecs: []string{"default=" + privateKey}})
	if err == nil || !strings.Contains(err.Error(), "requires --secret id=GIT_KNOWN_HOSTS") {
		t.Fatalf("missing known_hosts error = %v", err)
	}
}

func TestGitNamedContextDoesNotForwardAuthorizationAcrossHosts(t *testing.T) {
	destinationAuthorization := ""
	destination, _ := gitHTTPFixtureWithObserver(t, "", func(request *http.Request) {
		if authorization := request.Header.Get("Authorization"); authorization != "" {
			destinationAuthorization = authorization
		}
	})
	destinationURL, err := url.Parse(destination)
	if err != nil {
		t.Fatal(err)
	}
	origin := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		suffix := strings.TrimPrefix(request.URL.Path, "/repository.git")
		http.Redirect(writer, request, destinationURL.String()+suffix, http.StatusTemporaryRedirect)
	}))
	defer origin.Close()
	originURL, err := url.Parse(origin.URL)
	if err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(secret, []byte("origin-only-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = materializeGitNamedContext(context.Background(), buildcontext.Spec{
		Name: "private", Kind: buildcontext.Git, Reference: origin.URL + "/repository.git",
	}, v1.Platform{OS: "linux", Architecture: runtime.GOARCH}, nil, nil, buildCredentialSource{
		secretSpecs: []string{"id=GIT_AUTH_TOKEN." + originURL.Host + ",src=" + secret},
	})
	if err == nil || !strings.Contains(err.Error(), "authenticated") {
		t.Fatalf("authenticated redirect error = %v", err)
	}
	if destinationAuthorization != "" {
		t.Fatalf("Git named-context authorization leaked across hosts: %q", destinationAuthorization)
	}
}

func TestGitNamedContextVerifiesParentChecksumBeforeFetchingSubmodules(t *testing.T) {
	var submoduleRequests atomic.Int64
	source, _ := gitHTTPSubmoduleFixtureWithAuthorization(t, func(request *http.Request) bool {
		if strings.HasPrefix(request.URL.Path, "/submodule.git") || strings.HasPrefix(request.URL.Path, "/nested.git") {
			submoduleRequests.Add(1)
		}
		return true
	})
	_, err := materializeGitNamedContext(context.Background(), buildcontext.Spec{
		Name: "bad-pin", Kind: buildcontext.Git, Reference: source + "?checksum=" + strings.Repeat("0", 40),
	}, v1.Platform{OS: "linux", Architecture: runtime.GOARCH}, nil, nil, buildCredentialSource{})
	if err == nil || !strings.Contains(err.Error(), "unexpected commit") {
		t.Fatalf("bad parent checksum error = %v", err)
	}
	if requests := submoduleRequests.Load(); requests != 0 {
		t.Fatalf("bad parent checksum made %d submodule requests", requests)
	}
}

func TestGitNamedContextSelectorsDistinguishBranchAndTag(t *testing.T) {
	source, _ := gitProtocolFixture(t)
	for _, test := range []struct {
		selector, proof string
	}{
		{selector: "branch=release", proof: "release branch\n"},
		{selector: "branch=refs/heads/release", proof: "release branch\n"},
		{selector: "tag=release", proof: "default branch\n"},
		{selector: "tag=refs/tags/release", proof: "default branch\n"},
	} {
		t.Run(test.selector, func(t *testing.T) {
			options, err := normalizeGitNamedContextURL(source + "?" + test.selector)
			if err != nil {
				t.Fatal(err)
			}
			temporary, relative, err := define.TempDirForURL(os.TempDir(), "coopr-git-selector-", options.cloneSource)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(temporary) })
			data, err := os.ReadFile(filepath.Join(temporary, relative, "proof"))
			if err != nil || string(data) != test.proof {
				t.Fatalf("selected Git proof = %q, %v; want %q", data, err, test.proof)
			}
		})
	}
}

func TestExtractRemoteNamedContextConfinesArchivePaths(t *testing.T) {
	for _, test := range []struct {
		name   string
		header tar.Header
	}{
		{name: "parent traversal", header: tar.Header{Name: "../escape", Mode: 0o644}},
		{name: "absolute path", header: tar.Header{Name: "/escape", Mode: 0o644}},
		{name: "hard link traversal", header: tar.Header{Name: "link", Typeflag: tar.TypeLink, Linkname: "../escape", Mode: 0o644}},
	} {
		t.Run(test.name, func(t *testing.T) {
			parent := t.TempDir()
			directory := filepath.Join(parent, "context")
			if err := os.Mkdir(directory, 0o700); err != nil {
				t.Fatal(err)
			}
			var contents bytes.Buffer
			writer := tar.NewWriter(&contents)
			if err := writer.WriteHeader(&test.header); err != nil {
				t.Fatal(err)
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := extractRemoteNamedContext(context.Background(), directory, bytes.NewReader(contents.Bytes())); err == nil {
				t.Fatal("archive traversal was accepted")
			}
			if _, err := os.Lstat(filepath.Join(parent, "escape")); !os.IsNotExist(err) {
				t.Fatalf("archive escaped context root: %v", err)
			}
		})
	}
}

func TestExtractRemoteNamedContextDiscardsTrailingData(t *testing.T) {
	staging := t.TempDir()
	t.Setenv("TMPDIR", staging)
	archive := tarBytes(t, "proof", "context\n")
	input := &contextArchiveStageReader{
		Reader:  bytes.NewReader(append(archive, bytes.Repeat([]byte("trailer"), 65536)...)),
		staging: staging,
	}
	directory := t.TempDir()
	if _, err := extractRemoteNamedContext(context.Background(), directory, input); err != nil {
		t.Fatal(err)
	}
	if input.largest > int64(len(archive)) {
		t.Fatalf("staged archive grew to %d bytes for a %d-byte tar", input.largest, len(archive))
	}
	if contents, err := os.ReadFile(filepath.Join(directory, "proof")); err != nil || string(contents) != "context\n" {
		t.Fatalf("extracted proof = %q, %v", contents, err)
	}
	if entries, err := os.ReadDir(staging); err != nil || len(entries) != 0 {
		t.Fatalf("staging cleanup = %v, %v", entries, err)
	}
}

type contextArchiveStageReader struct {
	io.Reader
	staging string
	largest int64
}

func (reader *contextArchiveStageReader) Read(buffer []byte) (int, error) {
	entries, err := os.ReadDir(reader.staging)
	if err != nil {
		return 0, err
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "coopr-named-context-") {
			info, err := entry.Info()
			if err != nil {
				return 0, err
			}
			reader.largest = max(reader.largest, info.Size())
		}
	}
	return reader.Reader.Read(buffer)
}

func TestExtractRemoteNamedContextValidatesGzipFooter(t *testing.T) {
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	if _, err := writer.Write(tarBytes(t, "proof", "context\n")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	corrupt := bytes.Clone(compressed.Bytes())
	corrupt[len(corrupt)-8] ^= 1
	for _, test := range []struct {
		name string
		data []byte
		fail bool
	}{
		{name: "valid", data: compressed.Bytes()},
		{name: "bad checksum", data: corrupt, fail: true},
		{name: "truncated footer", data: compressed.Bytes()[:compressed.Len()-4], fail: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			staging := t.TempDir()
			t.Setenv("TMPDIR", staging)
			directory := t.TempDir()
			_, err := extractRemoteNamedContext(context.Background(), directory, bytes.NewReader(test.data))
			if (err != nil) != test.fail {
				t.Fatalf("extraction error = %v, want failure %t", err, test.fail)
			}
			if entries, err := os.ReadDir(staging); err != nil || len(entries) != 0 {
				t.Fatalf("staging cleanup = %v, %v", entries, err)
			}
		})
	}
}

func TestMaterializeHTTPNamedContextDoesNotForwardURLTokenOnRedirect(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping a live rootless named-context import in short mode")
	}
	archive := tarBytes(t, "proof", "redirected context\n")
	var refererMu sync.Mutex
	var referers []string
	destination := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		refererMu.Lock()
		referers = append(referers, request.Header.Get("Referer"))
		refererMu.Unlock()
		_, _ = writer.Write(archive)
	}))
	defer destination.Close()
	source := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, destination.URL+"/context.tar", http.StatusFound)
	}))
	defer source.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	root := t.TempDir()
	store, system := namedContextTestStore(t, root)
	_, err := MaterializeNamedContext(ctx, buildcontext.Spec{
		Name: "http", Kind: buildcontext.HTTPArchive,
		Reference: source.URL + "/context.tar?token=secret",
	}, v1.Platform{OS: "linux", Architecture: runtime.GOARCH}, store, system, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	refererMu.Lock()
	defer refererMu.Unlock()
	if len(referers) == 0 {
		t.Fatal("redirect destination was never requested")
	}
	for _, value := range referers {
		if value != "" {
			t.Fatalf("redirect leaked source URL in Referer: %q", value)
		}
	}
}

func TestMaterializeHTTPNamedContextRejectsNonSuccessStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusNotModified)
	}))
	defer server.Close()
	_, err := materializeHTTPNamedContext(context.Background(), buildcontext.Spec{
		Name: "http", Kind: buildcontext.HTTPArchive, Reference: server.URL + "/context.tar",
	}, v1.Platform{OS: "linux", Architecture: runtime.GOARCH}, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "304") {
		t.Fatalf("non-success response error = %v, want HTTP 304", err)
	}
}

func TestMaterializeHTTPNamedContextRejectsRedirectCredentials(t *testing.T) {
	contacted := make(chan struct{}, 1)
	destination := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		contacted <- struct{}{}
		writer.WriteHeader(http.StatusOK)
	}))
	defer destination.Close()
	source := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		target := strings.Replace(destination.URL, "http://", "http://user:password@", 1)
		http.Redirect(writer, request, target+"/context.tar", http.StatusFound)
	}))
	defer source.Close()
	_, err := materializeHTTPNamedContext(context.Background(), buildcontext.Spec{
		Name: "http", Kind: buildcontext.HTTPArchive, Reference: source.URL + "/context.tar",
	}, v1.Platform{OS: "linux", Architecture: runtime.GOARCH}, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "credentials") {
		t.Fatalf("redirect error = %v, want credential rejection", err)
	}
	select {
	case <-contacted:
		t.Fatal("credential-bearing redirect contacted the destination")
	default:
	}
}

func TestMaterializeLocalNamedContextFreezesFilteredFilesystem(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping a live rootless named-context import in short mode")
	}
	for _, test := range []struct {
		ignoreFile    string
		fallbackFiles []string
	}{
		{ignoreFile: ".cooprignore", fallbackFiles: []string{".containerignore", ".dockerignore"}},
		{ignoreFile: ".containerignore", fallbackFiles: []string{".dockerignore"}},
		{ignoreFile: ".dockerignore"},
	} {
		t.Run(test.ignoreFile, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			root := t.TempDir()
			contextDir := filepath.Join(root, "context")
			if err := os.Mkdir(contextDir, 0o750); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(contextDir, test.ignoreFile), []byte("ignored\n"), 0o640); err != nil {
				t.Fatal(err)
			}
			for _, name := range test.fallbackFiles {
				if err := os.WriteFile(filepath.Join(contextDir, name), []byte("kept\n"), 0o640); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(contextDir, "kept"), []byte("before\n"), 0o640); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(contextDir, "ignored"), []byte("secret\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			protected := filepath.Join(contextDir, "coopr-output")
			if err := os.WriteFile(protected, []byte("internal\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("kept", filepath.Join(contextDir, "alias")); err != nil {
				t.Fatal(err)
			}
			store, system := namedContextTestStore(t, root)
			platform := v1.Platform{OS: "linux", Architecture: runtime.GOARCH}
			resolved, err := MaterializeNamedContext(ctx, buildcontext.Spec{
				Name: "src", Kind: buildcontext.Local, Path: contextDir,
			}, platform, store, system, nil, nil, nil, protected)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(contextDir, "ignored"), []byte("changed secret\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			again, err := MaterializeNamedContext(ctx, buildcontext.Spec{
				Name: "src", Kind: buildcontext.Local, Path: contextDir,
			}, platform, store, system, nil, nil, nil, protected)
			if err != nil {
				t.Fatal(err)
			}
			if again.Selected.Digest != resolved.Selected.Digest {
				t.Fatalf("ignored file edit changed materialized digest: %s -> %s", resolved.Selected.Digest, again.Selected.Digest)
			}
			if err := os.WriteFile(filepath.Join(contextDir, test.ignoreFile), []byte("kept\n"), 0o640); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(contextDir, "kept"), []byte("after\n"), 0o640); err != nil {
				t.Fatal(err)
			}
			var config v1.Image
			if err := json.Unmarshal(resolved.ConfigData, &config); err != nil {
				t.Fatal(err)
			}
			if config.OS != "linux" || config.Architecture != platform.Architecture || config.RootFS.Type != "layers" || len(config.RootFS.DiffIDs) != 1 {
				t.Fatalf("local context config = %+v", config)
			}
			builder, err := upstream.NewBuilder(ctx, store, upstream.BuilderOptions{
				FromImage: resolved.ImageID, PullPolicy: define.PullNever, Isolation: define.IsolationChroot,
				Format: define.OCIv1ImageManifest, SystemContext: system,
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = builder.Delete() })
			mount, err := builder.Mount(builder.MountLabel)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = builder.Unmount() })
			data, err := os.ReadFile(filepath.Join(mount, "kept"))
			if err != nil || string(data) != "before\n" {
				t.Fatalf("frozen named-context file = %q, %v", data, err)
			}
			if _, err := os.Lstat(filepath.Join(mount, "ignored")); !os.IsNotExist(err) {
				t.Fatalf("ignored named-context file exists: %v", err)
			}
			if _, err := os.Lstat(filepath.Join(mount, "coopr-output")); !os.IsNotExist(err) {
				t.Fatalf("protected Coopr artifact exists in named context: %v", err)
			}
			link, err := os.Readlink(filepath.Join(mount, "alias"))
			if err != nil || link != "kept" {
				t.Fatalf("named-context symlink = %q, %v", link, err)
			}
		})
	}
}

func TestMaterializeRemoteNamedContexts(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live rootless remote named-context imports in short mode")
	}
	archive := tarBytes(t, "proof", "http context\n")
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write(archive)
	}))
	defer server.Close()
	gitSource, commit := gitHTTPSubmoduleFixture(t, "")
	identities := map[string]string{}

	for _, test := range []struct {
		name, path, proof, identity string
		spec                        buildcontext.Spec
		keepGitDir, skipSubmodules  bool
	}{
		{name: "HTTP archive", path: "proof", proof: "http context\n", spec: buildcontext.Spec{Name: "http", Kind: buildcontext.HTTPArchive, Reference: server.URL + "/context.tar"}},
		{name: "Git query with recursive submodule", path: "deps/submodule/deps/nested/nested-proof", proof: "nested submodule\n", identity: "default", spec: buildcontext.Spec{Name: "git", Kind: buildcontext.Git, Reference: gitSource + "?ref=" + commit + "&checksum=" + commit[:12]}},
		{name: "Git query without submodules", path: "proof", proof: "parent repository\n", identity: "skip", skipSubmodules: true, spec: buildcontext.Spec{Name: "git-skip", Kind: buildcontext.Git, Reference: gitSource + "?ref=" + commit + "&submodules=false"}},
		{name: "Git query retaining metadata", path: "deps/submodule/deps/nested/nested-proof", proof: "nested submodule\n", identity: "keep", keepGitDir: true, spec: buildcontext.Spec{Name: "git-keep", Kind: buildcontext.Git, Reference: gitSource + "?ref=" + commit + "&keep-git-dir=true"}},
		{name: "Git query with commit mtime", path: "deps/submodule/deps/nested/nested-proof", proof: "nested submodule\n", identity: "mtime", spec: buildcontext.Spec{Name: "git-mtime", Kind: buildcontext.Git, Reference: gitSource + "?ref=" + commit + "&mtime=commit"}},
		{name: "Git query fetch by commit after branch advances", path: "proof", proof: "parent repository\n", spec: buildcontext.Spec{Name: "git-fetch", Kind: buildcontext.Git, Reference: gitSource + "?ref=refs/heads/main&checksum=" + commit + "&fetch-by-commit"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			root := t.TempDir()
			store, system := namedContextTestStore(t, root)
			platform := v1.Platform{OS: "linux", Architecture: runtime.GOARCH}
			resolved, err := MaterializeNamedContext(ctx, test.spec, platform, store, system, nil, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			if test.identity != "" {
				identities[test.identity] = resolved.Selected.Digest.String()
			}
			builder, err := upstream.NewBuilder(ctx, store, upstream.BuilderOptions{
				FromImage: resolved.ImageID, PullPolicy: define.PullNever, Isolation: define.IsolationChroot,
				Format: define.OCIv1ImageManifest, SystemContext: system,
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = builder.Delete() })
			mount, err := builder.Mount(builder.MountLabel)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = builder.Unmount() })
			data, err := os.ReadFile(filepath.Join(mount, test.path))
			if err != nil || string(data) != test.proof {
				t.Fatalf("remote context proof = %q, %v", data, err)
			}
			_, rootGitErr := os.Lstat(filepath.Join(mount, ".git"))
			if test.keepGitDir && rootGitErr != nil || !test.keepGitDir && !os.IsNotExist(rootGitErr) {
				t.Fatalf("remote context .git state with keep-git-dir=%t: %v", test.keepGitDir, rootGitErr)
			}
			if test.spec.Kind == buildcontext.Git {
				for _, metadata := range []string{"deps/submodule/.git", "deps/submodule/deps/nested/.git"} {
					_, err := os.Lstat(filepath.Join(mount, metadata))
					wantMetadata := test.keepGitDir && !test.skipSubmodules
					if wantMetadata && err != nil || !wantMetadata && !os.IsNotExist(err) {
						t.Fatalf("remote context submodule metadata %q with keep-git-dir=%t submodules=%t: %v", metadata, test.keepGitDir, !test.skipSubmodules, err)
					}
				}
				if test.skipSubmodules {
					if _, err := os.Lstat(filepath.Join(mount, "deps/submodule/deps/nested/nested-proof")); !os.IsNotExist(err) {
						t.Fatalf("submodules=false populated nested submodule: %v", err)
					}
				}
			}
		})
	}
	if identities["default"] == "" || identities["skip"] == "" || identities["keep"] == "" || identities["mtime"] == "" ||
		identities["default"] == identities["skip"] || identities["default"] == identities["keep"] || identities["default"] == identities["mtime"] || identities["skip"] == identities["keep"] {
		t.Fatalf("Git named-context controls did not produce distinct immutable identities: %#v", identities)
	}
}

func TestBuildPlanConsumesRemoteNamedContexts(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live supervised remote named-context builds in short mode")
	}
	archive := tarBytes(t, "proof", "http context\n")
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write(archive)
	}))
	defer server.Close()
	gitSource, commit := gitProtocolFixture(t)
	gitHome := t.TempDir()
	gitConfig := filepath.Join(gitHome, ".gitconfig")
	if err := os.WriteFile(gitConfig, []byte("[url \"file:///nonexistent/coopr-git-config-hijack/\"]\n\tinsteadOf = "+gitSource+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitAttributesDir := filepath.Join(gitHome, ".config", "git")
	if err := os.MkdirAll(gitAttributesDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gitAttributesDir, "attributes"), []byte("proof text eol=crlf\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", gitHome)
	t.Setenv("GIT_CONFIG_GLOBAL", gitConfig)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(gitHome, ".config"))
	t.Setenv(gitWrapperDirectory, filepath.Join(gitHome, "fake-wrapper"))
	t.Setenv(realGitExecutable, "/bin/false")
	for _, test := range []struct {
		name, proof string
		spec        buildcontext.Spec
	}{
		{name: "HTTP archive", proof: "http context\n", spec: buildcontext.Spec{Name: "remote", Kind: buildcontext.HTTPArchive, Reference: server.URL + "/context.tar"}},
		{name: "Git repository", proof: "default branch\n", spec: buildcontext.Spec{Name: "remote", Kind: buildcontext.Git, Reference: gitSource + "?ref=" + commit + "&checksum=" + commit[:12]}},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			root := t.TempDir()
			layout := filepath.Join(root, "result")
			policy := writeComponentTestPolicy(t, root)
			plan := namedContextGraphPlan(t, `from "remote"`, test.spec)
			if _, err := BuildPlanSupervised(ctx, plan, SupervisedPlanOptions{
				Store:      StoreOptions{RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs"},
				ContextDir: root, Isolation: "rootless", Runtime: "crun", Output: Output{Path: layout},
				BuildContexts: []buildcontext.Spec{test.spec}, SignaturePolicyPath: policy, Stdout: io.Discard, Stderr: io.Discard,
			}); err != nil {
				t.Fatal(err)
			}
			manifest, _ := readPlanImage(t, layout)
			if len(manifest.Layers) != 1 {
				t.Fatalf("remote named-context layers = %d, want 1", len(manifest.Layers))
			}
			blob := filepath.Join(layout, "blobs", "sha256", manifest.Layers[0].Digest.Encoded())
			if got := readLayerFile(t, blob, "proof"); got != test.proof {
				t.Fatalf("remote named-context proof = %q", got)
			}
		})
	}
}

func TestBuildPlanConsumesAuthenticatedGitNamedContext(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping a live supervised private Git named-context build in short mode")
	}
	const token = "supervised-named-context-token"
	source, commit := gitHTTPFixture(t, "basic eC1hY2Nlc3MtdG9rZW46c3VwZXJ2aXNlZC1uYW1lZC1jb250ZXh0LXRva2Vu")
	parsed, err := url.Parse(source)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	secret := filepath.Join(root, "git-token")
	if err := os.WriteFile(secret, []byte(token), 0o600); err != nil {
		t.Fatal(err)
	}
	contextSpec := buildcontext.Spec{
		Name: "private", Kind: buildcontext.Git,
		Reference: source + "?ref=" + commit + "&checksum=" + commit[:12],
	}
	layout := filepath.Join(root, "result")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if _, err := BuildDefinitionSupervised(ctx, parseWorkerDefinition(t, `from "private"`), planner.Options{
		Mode: planner.Build, Platform: runtime.GOOS + "/" + runtime.GOARCH, BuildContexts: []buildcontext.Spec{contextSpec},
	}, SupervisedPlanOptions{
		Store:      StoreOptions{RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs"},
		ContextDir: root, Isolation: "rootless", Runtime: "crun", Output: Output{Path: layout},
		BuildContexts:       []buildcontext.Spec{contextSpec},
		Secrets:             []string{"id=GIT_AUTH_TOKEN." + parsed.Host + ",src=" + secret},
		SignaturePolicyPath: writeComponentTestPolicy(t, root), Stdout: io.Discard, Stderr: io.Discard,
	}); err != nil {
		t.Fatal(err)
	}
	manifest, _ := readPlanImage(t, layout)
	if len(manifest.Layers) != 1 {
		t.Fatalf("private Git named-context layers = %d, want 1", len(manifest.Layers))
	}
	blob := filepath.Join(layout, "blobs", "sha256", manifest.Layers[0].Digest.Encoded())
	if got := readLayerFile(t, blob, "proof"); got != "default branch\n" {
		t.Fatalf("private Git named-context proof = %q", got)
	}
}

func TestBuildPlanConsumesSSHGitNamedContext(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping a live supervised SSH Git named-context build in short mode")
	}
	source, privateKey, knownHosts := gitSSHFixture(t)
	root := t.TempDir()
	knownHostsPath := filepath.Join(root, "known_hosts")
	if err := os.WriteFile(knownHostsPath, knownHosts, 0o600); err != nil {
		t.Fatal(err)
	}
	contextSpec := buildcontext.Spec{Name: "private", Kind: buildcontext.Git, Reference: source + "?branch=main"}
	layout := filepath.Join(root, "result")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if _, err := BuildDefinitionSupervised(ctx, parseWorkerDefinition(t, `from "private"`), planner.Options{
		Mode: planner.Build, Platform: runtime.GOOS + "/" + runtime.GOARCH, BuildContexts: []buildcontext.Spec{contextSpec},
	}, SupervisedPlanOptions{
		Store:      StoreOptions{RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs"},
		ContextDir: root, Isolation: "rootless", Runtime: "crun", Output: Output{Path: layout},
		BuildContexts:       []buildcontext.Spec{contextSpec},
		Secrets:             []string{"id=GIT_KNOWN_HOSTS.127.0.0.1,src=" + knownHostsPath},
		SSH:                 []string{"default=" + privateKey},
		SignaturePolicyPath: writeComponentTestPolicy(t, root), Stdout: io.Discard, Stderr: io.Discard,
	}); err != nil {
		t.Fatal(err)
	}
	manifest, _ := readPlanImage(t, layout)
	if len(manifest.Layers) != 1 {
		t.Fatalf("SSH Git named-context layers = %d, want 1", len(manifest.Layers))
	}
	blob := filepath.Join(layout, "blobs", "sha256", manifest.Layers[0].Digest.Encoded())
	if got := readLayerFile(t, blob, "proof"); got != "ssh context\n" {
		t.Fatalf("SSH Git named-context proof = %q", got)
	}
}

func TestMaterializeOCILayoutNamedContextImportsSelectedPlatform(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping a live rootless named-context import in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	root := t.TempDir()
	layout := filepath.Join(root, "layout")
	layoutStore, err := orasoci.NewWithContext(ctx, layout)
	if err != nil {
		t.Fatal(err)
	}
	platform := v1.Platform{OS: "linux", Architecture: runtime.GOARCH}
	manifest, config := namedContextLayoutImage(t, ctx, layoutStore, platform)
	manifest.Platform = &platform
	indexData, err := json.Marshal(v1.Index{
		Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageIndex,
		Manifests: []v1.Descriptor{manifest},
	})
	if err != nil {
		t.Fatal(err)
	}
	index := oci.Descriptor(v1.MediaTypeImageIndex, indexData)
	if err := layoutStore.Push(ctx, index, bytes.NewReader(indexData)); err != nil {
		t.Fatal(err)
	}
	if err := layoutStore.Tag(ctx, index, "tools"); err != nil {
		t.Fatal(err)
	}
	store, system := namedContextTestStore(t, root)
	resolved, err := MaterializeNamedContext(ctx, buildcontext.Spec{
		Name: "tools", Kind: buildcontext.OCILayout, Path: layout, Reference: "tools",
	}, platform, store, system, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Root.Digest != index.Digest || resolved.Selected.Digest != manifest.Digest || resolved.ImageID != config.Digest.Encoded() {
		t.Fatalf("materialized OCI layout selection = %+v", resolved)
	}
	if image, err := store.Image(resolved.ImageID); err != nil || image.ID != resolved.ImageID {
		t.Fatalf("materialized OCI layout image = %+v, %v", image, err)
	}
}

func namedContextTestStore(t *testing.T, root string) (storage.Store, *types.SystemContext) {
	t.Helper()
	store, err := storage.GetStore(storage.StoreOptions{
		GraphDriverName: "vfs", GraphRoot: filepath.Join(root, "graph"), RunRoot: filepath.Join(root, "run"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := store.Shutdown(true); err != nil {
			t.Errorf("shutdown named-context store: %v", err)
		}
	})
	policy := filepath.Join(root, "policy.json")
	if err := os.WriteFile(policy, []byte(`{"default":[{"type":"insecureAcceptAnything"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return store, &types.SystemContext{SignaturePolicyPath: policy, BigFilesTemporaryDir: root}
}

func namedContextLayoutImage(t *testing.T, ctx context.Context, store *orasoci.Store, platform v1.Platform) (v1.Descriptor, v1.Descriptor) {
	t.Helper()
	configData, err := json.Marshal(v1.Image{Platform: platform, RootFS: v1.RootFS{Type: "layers"}, Config: v1.ImageConfig{Env: []string{"NAMED_CONTEXT=yes"}}})
	if err != nil {
		t.Fatal(err)
	}
	config := oci.Descriptor(v1.MediaTypeImageConfig, configData)
	if err := store.Push(ctx, config, bytes.NewReader(configData)); err != nil {
		t.Fatal(err)
	}
	manifestData, err := json.Marshal(v1.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageManifest, Config: config,
	})
	if err != nil {
		t.Fatal(err)
	}
	manifest := oci.Descriptor(v1.MediaTypeImageManifest, manifestData)
	if err := store.Push(ctx, manifest, bytes.NewReader(manifestData)); err != nil {
		t.Fatal(err)
	}
	return manifest, config
}

var _ content.ReadOnlyStorage = (*orasoci.Store)(nil)
