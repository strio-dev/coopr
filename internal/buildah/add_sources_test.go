package buildah

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	upstream "go.podman.io/buildah"
)

func TestAddUnpackFalseDisablesLocalArchiveExtraction(t *testing.T) {
	unpack := false
	builder := &recordingBuilder{}
	err := applyOperations(context.Background(), builder, t.TempDir(), []Operation{
		Add{Sources: []string{"archive.tar"}, Destination: "/archive", Unpack: &unpack},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(builder.events) != 1 || !strings.HasPrefix(builder.events[0], "copy:false:") {
		t.Fatalf("events = %#v", builder.events)
	}
}

func TestAddUnpackTrueDownloadsAndStagesRemoteArchive(t *testing.T) {
	archive := tarBytes(t, "proof", "remote unpack\n")
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests++
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write(archive)
	}))
	defer server.Close()

	unpack := true
	builder := &recordingBuilder{}
	err := applyOperations(context.Background(), builder, t.TempDir(), []Operation{Add{
		Sources: []string{server.URL + "/archive.tar"}, Destination: "/archive/", Unpack: &unpack,
		Checksum: digest.FromBytes(archive).String(),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if requests != 1 || len(builder.events) != 1 || !strings.HasPrefix(builder.events[0], "copy:true:") {
		t.Fatalf("requests = %d, events = %#v", requests, builder.events)
	}
	options := builder.addOptions[0]
	if options.Checksum != "" || options.ContextDir == "" {
		t.Fatalf("staged options = %#v", options)
	}
	if _, err := os.Stat(options.ContextDir); !os.IsNotExist(err) {
		t.Fatalf("temporary download directory still exists: %v", err)
	}
}

func TestAddUnpackTrueRejectsRemoteChecksumMismatch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = writer.Write([]byte("not expected"))
	}))
	defer server.Close()
	unpack := true
	err := applyOperations(context.Background(), &recordingBuilder{}, t.TempDir(), []Operation{Add{
		Sources: []string{server.URL + "/archive.tar"}, Destination: "/archive/", Unpack: &unpack,
		Checksum: digest.FromString("expected").String(),
	}})
	if err == nil || !strings.Contains(err.Error(), "unexpected response digest") {
		t.Fatalf("error = %v", err)
	}
}

func TestRemoteAddExtractionAndLastModified(t *testing.T) {
	modified := time.Date(2024, time.January, 2, 3, 4, 5, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Last-Modified", modified.Format(http.TimeFormat))
		_, _ = writer.Write([]byte("remote file\n"))
	}))
	defer server.Close()

	unpack := true
	for _, test := range []struct {
		name    string
		unpack  *bool
		extract bool
	}{
		{name: "default remote file"},
		{name: "explicit unpack", unpack: &unpack, extract: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			builder := &inspectingAddBuilder{}
			builder.inspect = func(_ string, extract bool, options upstream.AddAndCopyOptions, sources ...string) error {
				if extract != test.extract {
					t.Fatalf("extract = %t, want %t", extract, test.extract)
				}
				info, err := os.Stat(filepath.Join(options.ContextDir, sources[0]))
				if err != nil {
					return err
				}
				if !info.ModTime().Equal(modified) {
					t.Fatalf("download mtime = %s, want %s", info.ModTime(), modified)
				}
				return nil
			}
			if err := applyOperations(context.Background(), builder, t.TempDir(), []Operation{Add{
				Sources: []string{server.URL + "/payload"}, Destination: "/payload", Unpack: test.unpack,
			}}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRemoteAddAuthorizationUsesDestinationHostSecrets(t *testing.T) {
	builder := &credentialAddBuilder{secrets: map[string][]byte{
		"HTTP_AUTH_HEADER_source.example": []byte("Basic source"),
		"HTTP_AUTH_HEADER_target.example": []byte("Basic target"),
		"HTTP_AUTH_TOKEN_target.example":  []byte("target-token"),
	}}
	request, err := http.NewRequest(http.MethodGet, "https://source.example/archive.tar", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := setRemoteAddAuthorization(request, builder); err != nil {
		t.Fatal(err)
	}
	if got := request.Header.Get("Authorization"); got != "Basic source" {
		t.Fatalf("initial authorization = %q", got)
	}

	redirect, err := http.NewRequest(http.MethodGet, "https://target.example/archive.tar", nil)
	if err != nil {
		t.Fatal(err)
	}
	redirect.Header.Set("Authorization", request.Header.Get("Authorization"))
	redirect.Header.Set("Referer", "https://source.example/archive.tar?signature=do-not-leak")
	if err := remoteAddHTTPClient(builder).CheckRedirect(redirect, []*http.Request{request}); err != nil {
		t.Fatal(err)
	}
	if got := redirect.Header.Get("Authorization"); got != "Bearer target-token" {
		t.Fatalf("redirect authorization = %q", got)
	}
	if got := redirect.Header.Get("Referer"); got != "" {
		t.Fatalf("redirect leaked signed source URL through Referer: %q", got)
	}

	untrusted, err := http.NewRequest(http.MethodGet, "https://untrusted.example/archive.tar", nil)
	if err != nil {
		t.Fatal(err)
	}
	untrusted.Header.Set("Authorization", redirect.Header.Get("Authorization"))
	if err := remoteAddHTTPClient(builder).CheckRedirect(untrusted, []*http.Request{redirect}); err != nil {
		t.Fatal(err)
	}
	if got := untrusted.Header.Get("Authorization"); got != "" {
		t.Fatalf("authorization leaked to unconfigured host: %q", got)
	}
}

func TestRemoteAddRejectsExcessiveRedirects(t *testing.T) {
	builder := &credentialAddBuilder{}
	request, err := http.NewRequest(http.MethodGet, "https://example.invalid/archive.tar", nil)
	if err != nil {
		t.Fatal(err)
	}
	via := make([]*http.Request, 10)
	if err := remoteAddHTTPClient(builder).CheckRedirect(request, via); err == nil || !strings.Contains(err.Error(), "10 HTTP redirects") {
		t.Fatalf("redirect limit error = %v", err)
	}
}

func TestRemoteAddRejectsURLCredentialsBeforeContact(t *testing.T) {
	contacted := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		contacted <- struct{}{}
		writer.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	source := strings.Replace(server.URL, "http://", "http://user:password@", 1) + "/payload"
	err := applyOperations(context.Background(), &recordingBuilder{}, t.TempDir(), []Operation{Add{
		Sources: []string{source}, Destination: "/payload",
	}})
	if err == nil || !strings.Contains(err.Error(), "credentials") {
		t.Fatalf("source error = %v, want credential rejection", err)
	}
	if strings.Contains(err.Error(), "password") {
		t.Fatalf("source error exposed URL password: %v", err)
	}
	select {
	case <-contacted:
		t.Fatal("credential-bearing source contacted the server")
	default:
	}
}

func TestRemoteAddMalformedURLDoesNotExposeCredentials(t *testing.T) {
	err := applyOperations(context.Background(), &recordingBuilder{}, t.TempDir(), []Operation{Add{
		Sources: []string{"http://user:password%zz@example.invalid/payload"}, Destination: "/payload",
	}})
	if err == nil || !strings.Contains(err.Error(), "invalid ADD source URL") {
		t.Fatalf("malformed source error = %v", err)
	}
	if strings.Contains(err.Error(), "password") {
		t.Fatalf("malformed source error exposed URL password: %v", err)
	}
}

func TestRemoteAddRejectsRedirectCredentialsBeforeContact(t *testing.T) {
	contacted := make(chan struct{}, 1)
	destination := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		contacted <- struct{}{}
		writer.WriteHeader(http.StatusOK)
	}))
	defer destination.Close()
	source := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		target := strings.Replace(destination.URL, "http://", "http://user:password@", 1)
		http.Redirect(writer, request, target+"/payload", http.StatusFound)
	}))
	defer source.Close()

	err := applyOperations(context.Background(), &recordingBuilder{}, t.TempDir(), []Operation{Add{
		Sources: []string{source.URL + "/payload"}, Destination: "/payload",
	}})
	if err == nil || !strings.Contains(err.Error(), "credentials") {
		t.Fatalf("redirect error = %v, want credential rejection", err)
	}
	if strings.Contains(err.Error(), "password") {
		t.Fatalf("redirect error exposed URL password: %v", err)
	}
	select {
	case <-contacted:
		t.Fatal("credential-bearing redirect contacted the destination")
	default:
	}
}

func TestRemoteAddSendsHostScopedToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if got := request.Header.Get("Authorization"); got != "Bearer download-token" {
			t.Errorf("authorization = %q", got)
			writer.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = writer.Write([]byte("authenticated\n"))
	}))
	defer server.Close()

	builder := &credentialAddBuilder{secrets: map[string][]byte{
		"HTTP_AUTH_TOKEN_127.0.0.1": []byte("download-token"),
	}}
	if err := applyOperations(context.Background(), builder, t.TempDir(), []Operation{Add{
		Sources: []string{server.URL + "/payload"}, Destination: "/payload",
	}}); err != nil {
		t.Fatal(err)
	}
	if len(builder.events) != 1 {
		t.Fatalf("events = %#v", builder.events)
	}
}

func TestGitAddSourceRecognition(t *testing.T) {
	for _, source := range []string{
		"https://example.invalid/repository.git#main",
		"http://example.invalid/repository.git",
		"git://example.invalid/repository.git",
		"ssh://git@example.invalid/repository.git",
		"git@example.invalid:repository.git#main",
	} {
		if !isGitAddSource(source) {
			t.Errorf("%q was not recognized as Git", source)
		}
	}
	for _, source := range []string{"https://example.invalid/archive.tar", "repository.git", "source"} {
		if isGitAddSource(source) {
			t.Errorf("%q was recognized as remote Git", source)
		}
	}
}

func TestGitAddSSHRequiresExplicitDefaultSource(t *testing.T) {
	for _, source := range []string{
		"ssh://git@example.invalid/repository.git",
		"git@example.invalid:repository.git#main",
	} {
		err := applyOperations(context.Background(), &sshAddBuilder{}, t.TempDir(), []Operation{Add{
			Sources: []string{source}, Destination: "/source/",
		}})
		if err == nil || !strings.Contains(err.Error(), "requires --ssh default") {
			t.Errorf("source %q: error = %v", source, err)
		}
	}
}

func TestGitAddEnvironmentUsesExplicitSSHSource(t *testing.T) {
	cleaned := false
	builder := &sshAddBuilder{socket: "/tmp/coopr-test-agent.sock", secrets: map[string][]byte{
		"GIT_KNOWN_HOSTS.example.invalid": []byte("example.invalid ssh-ed25519 AAAATEST\n"),
	}, cleanup: func() error {
		cleaned = true
		return nil
	}}
	environment, cleanup, authenticated, err := gitAddEnvironment(builder, "git@example.invalid:repository.git")
	if err != nil {
		t.Fatal(err)
	}
	if authenticated {
		t.Fatal("SSH environment was marked as HTTP-secret authenticated")
	}
	if got := environmentValue(environment, "SSH_AUTH_SOCK"); got != builder.socket {
		t.Fatalf("SSH_AUTH_SOCK = %q", got)
	}
	if command := environmentValue(environment, "GIT_SSH_COMMAND"); strings.Contains(command, "StrictHostKeyChecking=no") ||
		!strings.Contains(command, "StrictHostKeyChecking=yes") || !strings.Contains(command, "GlobalKnownHostsFile=/dev/null") ||
		!strings.Contains(command, "UserKnownHostsFile=") {
		t.Fatalf("GIT_SSH_COMMAND = %q", command)
	}
	knownHosts := strings.Trim(environmentValue(environment, "GIT_SSH_COMMAND")[strings.LastIndex(environmentValue(environment, "GIT_SSH_COMMAND"), "=")+1:], "'")
	if contents, err := os.ReadFile(knownHosts); err != nil || string(contents) != "example.invalid ssh-ed25519 AAAATEST\n" {
		t.Fatalf("known_hosts contents = %q, error = %v", contents, err)
	}
	if cleanup == nil {
		t.Fatal("SSH environment has no cleanup")
	}
	if err := cleanup(); err != nil {
		t.Fatal(err)
	}
	if !cleaned {
		t.Fatal("SSH agent cleanup was not called")
	}
	if _, err := os.Stat(knownHosts); !os.IsNotExist(err) {
		t.Fatalf("known_hosts temporary file still exists: %v", err)
	}
}

func TestGitAddEnvironmentUsesBuildKitAuthSecrets(t *testing.T) {
	remote := "https://git.example.invalid/team/repository.git"
	builder := &credentialAddBuilder{secrets: map[string][]byte{
		"GIT_AUTH_TOKEN.git.example.invalid": []byte("private-token"),
		"GIT_AUTH_TOKEN":                     []byte("wrong-fallback"),
	}}
	environment, cleanup, authenticated, err := gitAddEnvironment(builder, remote)
	if err != nil {
		t.Fatal(err)
	}
	if cleanup != nil {
		t.Fatal("HTTPS Git auth unexpectedly returned cleanup")
	}
	if !authenticated {
		t.Fatal("HTTPS secret auth was not marked authenticated")
	}
	if got := environmentValue(environment, "GIT_CONFIG_KEY_0"); got != "http."+remote+".extraHeader" {
		t.Fatalf("Git config key = %q", got)
	}
	want := "Authorization: basic eC1hY2Nlc3MtdG9rZW46cHJpdmF0ZS10b2tlbg=="
	if got := environmentValue(environment, "GIT_CONFIG_VALUE_0"); got != want {
		t.Fatalf("Git authorization = %q, want %q", got, want)
	}
}

func TestGitAddEnvironmentUsesRawAuthorizationHeaderSecret(t *testing.T) {
	remote := "https://git.example.invalid/team/repository.git"
	builder := &credentialAddBuilder{secrets: map[string][]byte{
		"GIT_AUTH_HEADER.git.example.invalid": []byte("Bearer opaque-token"),
		"GIT_AUTH_TOKEN.git.example.invalid":  []byte("lower-priority-token"),
	}}
	environment, _, authenticated, err := gitAddEnvironment(builder, remote)
	if err != nil {
		t.Fatal(err)
	}
	if !authenticated {
		t.Fatal("raw header secret was not marked authenticated")
	}
	if got := environmentValue(environment, "GIT_CONFIG_VALUE_0"); got != "Authorization: Bearer opaque-token" {
		t.Fatalf("Git authorization = %q", got)
	}
}

func TestGitAddEnvironmentRejectsHeaderInjection(t *testing.T) {
	const secret = "GIT_AUTH_HEADER.git.example.invalid"
	builder := &credentialAddBuilder{secrets: map[string][]byte{
		secret: []byte("Bearer token\r\nX-Injected: true"),
	}}
	_, _, _, err := gitAddEnvironment(builder, "https://git.example.invalid/repository.git")
	if err == nil || !strings.Contains(err.Error(), "prohibited control character") {
		t.Fatalf("header injection error = %v", err)
	}
	if strings.Contains(err.Error(), "X-Injected") {
		t.Fatalf("header injection error exposed secret contents: %v", err)
	}
}

func TestGitAddUsesHostSelectedToken(t *testing.T) {
	const token = "git-token"
	source, _ := gitHTTPFixture(t, "basic eC1hY2Nlc3MtdG9rZW46Z2l0LXRva2Vu")
	parsed, err := url.Parse(source)
	if err != nil {
		t.Fatal(err)
	}
	builder := &credentialAddBuilder{secrets: map[string][]byte{
		"GIT_AUTH_TOKEN." + parsed.Host: []byte(token),
	}}
	if err := applyOperations(context.Background(), builder, t.TempDir(), []Operation{Add{
		Sources: []string{source}, Destination: "/source/",
	}}); err != nil {
		t.Fatal(err)
	}
	if len(builder.events) != 1 {
		t.Fatalf("events = %#v", builder.events)
	}
}

func TestGitAddAuthenticationFailureDoesNotExposeSecret(t *testing.T) {
	const secret = "do-not-print-this-token"
	source, _ := gitHTTPFixture(t, "Bearer some-other-token")
	parsed, err := url.Parse(source)
	if err != nil {
		t.Fatal(err)
	}
	builder := &credentialAddBuilder{secrets: map[string][]byte{
		"GIT_AUTH_HEADER." + parsed.Host: []byte("Bearer " + secret),
	}}
	err = applyOperations(context.Background(), builder, t.TempDir(), []Operation{Add{
		Sources: []string{source}, Destination: "/source/",
	}})
	if err == nil {
		t.Fatal("authenticated Git ADD unexpectedly succeeded")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("Git ADD error exposed secret: %v", err)
	}
}

func TestGitAddDoesNotForwardAuthorizationAcrossHosts(t *testing.T) {
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
		location := destinationURL.String() + suffix
		if request.URL.RawQuery != "" {
			location += "?" + request.URL.RawQuery
		}
		http.Redirect(writer, request, location, http.StatusTemporaryRedirect)
	}))
	defer origin.Close()
	originURL, err := url.Parse(origin.URL)
	if err != nil {
		t.Fatal(err)
	}
	builder := &credentialAddBuilder{secrets: map[string][]byte{
		"GIT_AUTH_TOKEN." + originURL.Host: []byte("origin-only-token"),
	}}
	err = applyOperations(context.Background(), builder, t.TempDir(), []Operation{Add{
		Sources: []string{origin.URL + "/repository.git"}, Destination: "/source/",
	}})
	if err == nil || !strings.Contains(err.Error(), "authenticated ADD Git source") {
		t.Fatalf("authenticated redirect error = %v", err)
	}
	if destinationAuthorization != "" {
		t.Fatalf("Git authorization leaked across hosts: %q", destinationAuthorization)
	}
}

func TestParseGitAddSourceRejectsCredentialAndEscapingSubdirectory(t *testing.T) {
	for _, source := range []string{
		"https://user:password@example.invalid/repository.git",
		"ssh://git:password@example.invalid/repository.git",
		"https://example.invalid/repository.git#main:../outside",
		"https://example.invalid/repository.git#main:/outside",
	} {
		_, _, _, err := parseGitAddSource(source)
		if err == nil {
			t.Errorf("source %q was accepted", source)
		}
		if strings.Contains(fmt.Sprint(err), "password") {
			t.Errorf("source %q leaked password in error: %v", source, err)
		}
	}
	remote, _, _, err := parseGitAddSource("ssh://git@example.invalid/repository.git#main")
	if err != nil || remote != "ssh://git@example.invalid/repository.git" {
		t.Fatalf("SSH username URL parsed as %q, error = %v", remote, err)
	}
}

func TestGitAddDefaultRefChecksumAndMetadataOptions(t *testing.T) {
	source, commit := gitProtocolFixture(t)

	for _, test := range []struct {
		name       string
		keepGitDir bool
	}{
		{name: "metadata removed by default"},
		{name: "metadata retained", keepGitDir: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			keepGitDir := test.keepGitDir
			builder := &inspectingAddBuilder{}
			builder.inspect = func(destination string, extract bool, options upstream.AddAndCopyOptions, sources ...string) error {
				if destination != "/source/" || extract || len(sources) != 1 || sources[0] != "." {
					t.Fatalf("ADD call = destination %q, extract %t, sources %#v", destination, extract, sources)
				}
				contents, err := os.ReadFile(filepath.Join(options.ContextDir, "proof"))
				if err != nil {
					return err
				}
				if string(contents) != "default branch\n" {
					t.Fatalf("cloned proof = %q", contents)
				}
				excludesGit := strings.Contains(strings.Join(options.Excludes, ","), ".git")
				if excludesGit == test.keepGitDir {
					t.Fatalf("keep-git-dir %t produced excludes %#v", test.keepGitDir, options.Excludes)
				}
				return nil
			}

			err := applyOperations(context.Background(), builder, t.TempDir(), []Operation{Add{
				Sources: []string{source}, Destination: "/source/", Checksum: commit[:12], KeepGitDir: &keepGitDir,
			}})
			if err != nil {
				t.Fatal(err)
			}
			if len(builder.addOptions) != 1 {
				t.Fatalf("ADD calls = %d", len(builder.addOptions))
			}
			if _, err := os.Stat(builder.addOptions[0].ContextDir); !os.IsNotExist(err) {
				t.Fatalf("temporary Git clone still exists: %v", err)
			}
		})
	}

	err := applyOperations(context.Background(), &recordingBuilder{}, t.TempDir(), []Operation{Add{
		Sources: []string{source}, Destination: "/source/", Checksum: strings.Repeat("0", 12),
	}})
	if err == nil || !strings.Contains(err.Error(), "unexpected commit") {
		t.Fatalf("checksum mismatch error = %v", err)
	}
}

func TestGitAddRecursivelyChecksOutSubmodules(t *testing.T) {
	source, _ := gitHTTPSubmoduleFixture(t, "")
	builder := &inspectingAddBuilder{}
	builder.inspect = func(_ string, _ bool, options upstream.AddAndCopyOptions, _ ...string) error {
		contents, err := os.ReadFile(filepath.Join(options.ContextDir, "deps", "submodule", "deps", "nested", "nested-proof"))
		if err != nil {
			return err
		}
		if string(contents) != "nested submodule\n" {
			t.Fatalf("submodule proof = %q", contents)
		}
		return nil
	}
	if err := applyOperations(context.Background(), builder, t.TempDir(), []Operation{Add{
		Sources: []string{source}, Destination: "/source/",
	}}); err != nil {
		t.Fatal(err)
	}
}

func TestGitAddVerifiesParentChecksumBeforeFetchingSubmodules(t *testing.T) {
	var submoduleRequests atomic.Int64
	source, _ := gitHTTPSubmoduleFixtureWithAuthorization(t, func(request *http.Request) bool {
		if strings.HasPrefix(request.URL.Path, "/submodule.git") || strings.HasPrefix(request.URL.Path, "/nested.git") {
			submoduleRequests.Add(1)
		}
		return true
	})
	err := applyOperations(context.Background(), &recordingBuilder{}, t.TempDir(), []Operation{Add{
		Sources: []string{source}, Destination: "/source/", Checksum: strings.Repeat("0", 40),
	}})
	if err == nil || !strings.Contains(err.Error(), "unexpected commit") {
		t.Fatalf("bad parent checksum error = %v", err)
	}
	if requests := submoduleRequests.Load(); requests != 0 {
		t.Fatalf("bad parent checksum made %d submodule requests", requests)
	}
}

func TestGitAddDoesNotForwardRepositoryScopedAuthToSiblingSubmodules(t *testing.T) {
	const authorization = "Bearer recursive-token"
	submoduleAuthorization := "not requested"
	source, _ := gitHTTPSubmoduleFixtureWithAuthorization(t, func(request *http.Request) bool {
		if strings.HasPrefix(request.URL.Path, "/repository.git") {
			return request.Header.Get("Authorization") == authorization
		}
		submoduleAuthorization = request.Header.Get("Authorization")
		return submoduleAuthorization == ""
	})
	parsed, err := url.Parse(source)
	if err != nil {
		t.Fatal(err)
	}
	builder := &credentialAddBuilder{secrets: map[string][]byte{
		"GIT_AUTH_HEADER." + parsed.Host: []byte(authorization),
	}}
	if err := applyOperations(context.Background(), builder, t.TempDir(), []Operation{Add{
		Sources: []string{source}, Destination: "/source/",
	}}); err != nil {
		t.Fatal(err)
	}
	if submoduleAuthorization != "" {
		t.Fatalf("repository authorization leaked to sibling submodule: %q", submoduleAuthorization)
	}
}

func TestGitAuthorizationScopeIsRepositorySpecific(t *testing.T) {
	for _, test := range []struct{ remote, want string }{
		{remote: "https://git.example.invalid/team/repository.git", want: "https://git.example.invalid/team/repository.git"},
		{remote: "https://github.com/team/repository.git", want: "https://github.com/team/repository.git"},
		{remote: "https://www.github.com/team/repository.git", want: "https://www.github.com/team/repository.git"},
	} {
		if got := gitAuthorizationScope(test.remote); got != test.want {
			t.Errorf("gitAuthorizationScope(%q) = %q, want %q", test.remote, got, test.want)
		}
	}
}

func TestGitAddOptionsRemoveMetadataByDefault(t *testing.T) {
	base := upstream.AddAndCopyOptions{Checksum: "abcdef", IgnoreFile: "/context/.dockerignore", Excludes: []string{"*.tmp"}}
	withoutGit := gitAddOptions(base, "/clone", nil)
	if withoutGit.Checksum != "" || withoutGit.IgnoreFile != "" || withoutGit.ContextDir != "/clone" ||
		!strings.Contains(strings.Join(withoutGit.Excludes, ","), ".git") {
		t.Fatalf("default Git options = %#v", withoutGit)
	}
	keep := true
	withGit := gitAddOptions(base, "/clone", &keep)
	if strings.Contains(strings.Join(withGit.Excludes, ","), ".git") {
		t.Fatalf("keep-git-dir options = %#v", withGit)
	}
}

func TestVerifyGitCommitAcceptsPrefixAndRejectsMismatch(t *testing.T) {
	repository := t.TempDir()
	if err := os.Mkdir(filepath.Join(repository, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	const commit = "0123456789abcdef0123456789abcdef01234567"
	if err := os.WriteFile(filepath.Join(repository, ".git", "HEAD"), []byte(commit+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := verifyGitCommit(repository, "0123456"); err != nil {
		t.Fatal(err)
	}
	if err := verifyGitCommit(repository, "fedcba"); err == nil || !strings.Contains(err.Error(), "unexpected commit") {
		t.Fatalf("mismatch error = %v", err)
	}
}

type inspectingAddBuilder struct {
	recordingBuilder
	inspect func(string, bool, upstream.AddAndCopyOptions, ...string) error
}

type credentialAddBuilder struct {
	recordingBuilder
	secrets map[string][]byte
}

type sshAddBuilder struct {
	recordingBuilder
	socket  string
	cleanup func() error
	secrets map[string][]byte
}

func (builder *sshAddBuilder) addSourceSSH(id string) (string, func() error, bool, error) {
	if id != "default" || builder.socket == "" {
		return "", nil, false, nil
	}
	return builder.socket, builder.cleanup, true, nil
}

func (builder *sshAddBuilder) addSourceSecret(id string) ([]byte, bool, error) {
	value, found := builder.secrets[id]
	return append([]byte(nil), value...), found, nil
}

func (builder *credentialAddBuilder) addSourceSecret(id string) ([]byte, bool, error) {
	value, found := builder.secrets[id]
	return append([]byte(nil), value...), found, nil
}

func (builder *inspectingAddBuilder) add(destination string, extract bool, options upstream.AddAndCopyOptions, sources ...string) error {
	if err := builder.inspect(destination, extract, options, sources...); err != nil {
		return err
	}
	return builder.recordingBuilder.add(destination, extract, options, sources...)
}

func gitProtocolFixture(t *testing.T) (string, string) {
	return gitHTTPFixture(t, "")
}

func gitHTTPFixture(t *testing.T, expectedAuthorization string) (string, string) {
	return gitHTTPFixtureWithObserver(t, expectedAuthorization, nil)
}

func gitHTTPFixtureWithObserver(t *testing.T, expectedAuthorization string, observe func(*http.Request)) (string, string) {
	t.Helper()
	root := t.TempDir()
	working := filepath.Join(root, "working")
	runGit(t, "init", "-b", "main", working)
	runGit(t, "-C", working, "config", "user.name", "Coopr Test")
	runGit(t, "-C", working, "config", "user.email", "coopr@example.invalid")
	if err := os.WriteFile(filepath.Join(working, "proof"), []byte("default branch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, "-C", working, "add", "proof")
	runGit(t, "-C", working, "commit", "-m", "fixture")
	commit := strings.TrimSpace(runGit(t, "-C", working, "rev-parse", "HEAD"))
	runGit(t, "-C", working, "switch", "-c", "release")
	if err := os.WriteFile(filepath.Join(working, "proof"), []byte("release branch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, "-C", working, "add", "proof")
	runGit(t, "-C", working, "commit", "-m", "release branch")
	runGit(t, "-C", working, "tag", "release", commit)
	runGit(t, "-C", working, "switch", "main")

	repository := filepath.Join(root, "repository.git")
	runGit(t, "clone", "--bare", working, repository)

	backend, err := exec.LookPath("git-http-backend")
	if err != nil {
		t.Fatal(err)
	}
	backendHandler := &cgi.Handler{
		Path: backend,
		Root: "/",
		Env:  []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1"},
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if observe != nil {
			observe(request)
		}
		if expectedAuthorization != "" && request.Header.Get("Authorization") != expectedAuthorization {
			writer.WriteHeader(http.StatusUnauthorized)
			return
		}
		backendHandler.ServeHTTP(writer, request)
	}))
	t.Cleanup(server.Close)
	return server.URL + "/repository.git", commit
}

func gitHTTPSubmoduleFixture(t *testing.T, expectedAuthorization string) (string, string) {
	return gitHTTPSubmoduleFixtureWithAuthorization(t, func(request *http.Request) bool {
		return expectedAuthorization == "" || request.Header.Get("Authorization") == expectedAuthorization
	})
}

func gitHTTPSubmoduleFixtureWithAuthorization(t *testing.T, authorized func(*http.Request) bool) (string, string) {
	t.Helper()
	root := t.TempDir()
	nestedWorking := filepath.Join(root, "nested-working")
	runGit(t, "init", "-b", "main", nestedWorking)
	runGit(t, "-C", nestedWorking, "config", "user.name", "Coopr Test")
	runGit(t, "-C", nestedWorking, "config", "user.email", "coopr@example.invalid")
	if err := os.WriteFile(filepath.Join(nestedWorking, "nested-proof"), []byte("nested submodule\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, "-C", nestedWorking, "add", "nested-proof")
	runGit(t, "-C", nestedWorking, "commit", "-m", "nested submodule fixture")
	nestedCommit := strings.TrimSpace(runGit(t, "-C", nestedWorking, "rev-parse", "HEAD"))
	runGit(t, "clone", "--bare", nestedWorking, filepath.Join(root, "nested.git"))

	submoduleWorking := filepath.Join(root, "submodule-working")
	runGit(t, "init", "-b", "main", submoduleWorking)
	runGit(t, "-C", submoduleWorking, "config", "user.name", "Coopr Test")
	runGit(t, "-C", submoduleWorking, "config", "user.email", "coopr@example.invalid")
	if err := os.WriteFile(filepath.Join(submoduleWorking, "submodule-proof"), []byte("recursive submodule\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitmodules := "[submodule \"deps/nested\"]\n\tpath = deps/nested\n\turl = ../nested.git\n"
	if err := os.WriteFile(filepath.Join(submoduleWorking, ".gitmodules"), []byte(gitmodules), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, "-C", submoduleWorking, "add", "submodule-proof", ".gitmodules")
	runGit(t, "-C", submoduleWorking, "update-index", "--add", "--cacheinfo", "160000,"+nestedCommit+",deps/nested")
	runGit(t, "-C", submoduleWorking, "commit", "-m", "submodule fixture")
	submoduleCommit := strings.TrimSpace(runGit(t, "-C", submoduleWorking, "rev-parse", "HEAD"))
	runGit(t, "clone", "--bare", submoduleWorking, filepath.Join(root, "submodule.git"))

	working := filepath.Join(root, "working")
	runGit(t, "init", "-b", "main", working)
	runGit(t, "-C", working, "config", "user.name", "Coopr Test")
	runGit(t, "-C", working, "config", "user.email", "coopr@example.invalid")
	if err := os.WriteFile(filepath.Join(working, "proof"), []byte("parent repository\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitmodules = "[submodule \"deps/submodule\"]\n\tpath = deps/submodule\n\turl = ../submodule.git\n"
	if err := os.WriteFile(filepath.Join(working, ".gitmodules"), []byte(gitmodules), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, "-C", working, "add", "proof", ".gitmodules")
	runGit(t, "-C", working, "update-index", "--add", "--cacheinfo", "160000,"+submoduleCommit+",deps/submodule")
	runGitWithEnvironment(t, []string{"GIT_AUTHOR_DATE=@946684800", "GIT_COMMITTER_DATE=@946684800"}, "-C", working, "commit", "-m", "parent fixture")
	commit := strings.TrimSpace(runGit(t, "-C", working, "rev-parse", "HEAD"))
	if err := os.WriteFile(filepath.Join(working, "proof"), []byte("advanced branch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, "-C", working, "add", "proof")
	runGitWithEnvironment(t, []string{"GIT_AUTHOR_DATE=@946684801", "GIT_COMMITTER_DATE=@946684801"}, "-C", working, "commit", "-m", "advance parent branch")
	runGit(t, "clone", "--bare", working, filepath.Join(root, "repository.git"))

	backend, err := exec.LookPath("git-http-backend")
	if err != nil {
		t.Fatal(err)
	}
	backendHandler := &cgi.Handler{
		Path: backend,
		Root: "/",
		Env:  []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1"},
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if !authorized(request) {
			writer.WriteHeader(http.StatusUnauthorized)
			return
		}
		backendHandler.ServeHTTP(writer, request)
	}))
	t.Cleanup(server.Close)
	return server.URL + "/repository.git", commit
}

func environmentValue(environment []string, name string) string {
	prefix := name + "="
	for index := len(environment) - 1; index >= 0; index-- {
		if value, found := strings.CutPrefix(environment[index], prefix); found {
			return value
		}
	}
	return ""
}

func runGit(t *testing.T, arguments ...string) string {
	return runGitWithEnvironment(t, nil, arguments...)
}

func runGitWithEnvironment(t *testing.T, environment []string, arguments ...string) string {
	t.Helper()
	command := exec.Command("git", arguments...)
	command.Env = append(os.Environ(), environment...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(arguments, " "), err, output)
	}
	return string(output)
}

func tarBytes(t *testing.T, name, contents string) []byte {
	t.Helper()
	buffer := &bytes.Buffer{}
	writer := tar.NewWriter(buffer)
	if err := writer.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(contents))}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte(contents)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func TestRemoteSourceNameRequiresPath(t *testing.T) {
	_, err := remoteSourceName("https://example.invalid")
	if err == nil || !strings.Contains(err.Error(), "has no filename") {
		t.Fatalf("error = %v", err)
	}
	name, err := remoteSourceName("https://example.invalid/path/archive.tar?download=1")
	if err != nil || name != "archive.tar" {
		t.Fatalf("name = %q, error = %v", name, err)
	}
}

func TestAddChecksumRequiresOneSource(t *testing.T) {
	err := applyOperations(context.Background(), &recordingBuilder{}, t.TempDir(), []Operation{Add{
		Sources: []string{"one", "two"}, Destination: "/sources/", Checksum: digest.FromString("value").String(),
	}})
	if err == nil || !strings.Contains(err.Error(), "ADD checksum requires exactly one source") {
		t.Fatalf("error = %v", err)
	}
}

func TestAddMultipleSourcesDefersDestinationTypeToBuilder(t *testing.T) {
	builder := &recordingBuilder{}
	err := applyOperations(context.Background(), builder, t.TempDir(), []Operation{Add{
		Sources: []string{"one", "two"}, Destination: "/existing-directory",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(builder.events) != 1 || !strings.Contains(builder.events[0], ":/existing-directory:") {
		t.Fatalf("events = %#v", builder.events)
	}
}

func TestSplitMultipleSourceDestinationResolvesAgainstWorkDir(t *testing.T) {
	builder := &recordingBuilder{workDir: "/work", user: "1000"}
	if err := ensureSplitMultipleSourceDestination(builder, "target", ""); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(builder.events, []string{"mkdir-dir:/work/target:"}) {
		t.Fatalf("events = %#v", builder.events)
	}
	builder.events = nil
	if err := ensureSplitMultipleSourceDestination(builder, "owned", "12:34"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(builder.events, []string{"mkdir-dir:/work/owned:12:34"}) {
		t.Fatalf("explicit chown events = %#v", builder.events)
	}
}
