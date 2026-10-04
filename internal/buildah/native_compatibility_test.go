package buildah

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/docker/go-connections/nat"
	upstream "go.podman.io/buildah"
	"go.podman.io/buildah/define"
	"go.podman.io/image/v5/types"
)

func TestRemoteAddTimestampCompatibility(t *testing.T) {
	stamp := time.Date(2020, 2, 3, 4, 5, 6, 0, time.FixedZone("offset", 3600))
	for _, tc := range []struct {
		name, header string
		stamp        *time.Time
		want         time.Time
		invalid      bool
	}{
		{name: "absent", want: time.Unix(0, 0)},
		{name: "malformed", header: "yesterday", invalid: true},
		{name: "obsolete HTTP date", header: "Sunday, 06-Nov-94 08:49:37 GMT", invalid: true},
		{name: "override absent", stamp: &stamp, want: stamp},
		{name: "override malformed", header: "yesterday", stamp: &stamp, want: stamp},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Last-Modified", tc.header)
				_, _ = w.Write([]byte("payload"))
			}))
			defer server.Close()
			called := false
			builder := &inspectingAddBuilder{inspect: func(_ string, _ bool, options upstream.AddAndCopyOptions, sources ...string) error {
				called = true
				info, err := os.Stat(filepath.Join(options.ContextDir, sources[0]))
				if err != nil {
					return err
				}
				if !info.ModTime().Equal(tc.want) {
					t.Fatalf("mtime=%v want=%v", info.ModTime(), tc.want)
				}
				return nil
			}}
			err := applyRemoteAddSource(context.Background(), builder, Add{Destination: "/payload"}, upstream.AddAndCopyOptions{Timestamp: tc.stamp}, server.URL+"/payload")
			if tc.invalid {
				if err == nil || !strings.Contains(err.Error(), "last-modified") || called {
					t.Fatalf("error=%v called=%v", err, called)
				}
				return
			}
			if err != nil || !called {
				t.Fatalf("error=%v called=%v", err, called)
			}
		})
	}
}

func TestRemoteAddRootURLExplicitFile(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("payload")) }))
	defer server.Close()
	called := false
	builder := &inspectingAddBuilder{inspect: func(destination string, _ bool, options upstream.AddAndCopyOptions, sources ...string) error {
		called = true
		if destination != "/payload" {
			t.Fatalf("destination=%q", destination)
		}
		got, err := os.ReadFile(filepath.Join(options.ContextDir, sources[0]))
		if err != nil {
			return err
		}
		if string(got) != "payload" {
			t.Fatalf("payload=%q", got)
		}
		return nil
	}}
	if err := applyRemoteAddSource(context.Background(), builder, Add{Destination: "/payload"}, upstream.AddAndCopyOptions{}, server.URL+"/"); err != nil || !called {
		t.Fatalf("error=%v called=%v", err, called)
	}
	for _, destination := range []string{"/directory/", "/directory/.", "."} {
		err := applyRemoteAddSource(context.Background(), builder, Add{Destination: destination}, upstream.AddAndCopyOptions{}, server.URL+"/")
		if err == nil || !strings.Contains(err.Error(), "has no filename") {
			t.Fatalf("destination %q error=%v", destination, err)
		}
	}
}

func TestExposePinnedPortGrammarDifferential(t *testing.T) {
	for _, raw := range []string{"80", "81/UDP", "82-83/sCtP", "127.0.0.1:9000:84", "9000-9002:80", "9000-9001:80-81", "[::1]:9000:80", "::1:9000:80", "0", "65535", "65536", "-1", "90-80", "8000-8002:80-81", "not-an-ip:8000:80", "[::gg]:9000:80", "80/http", "/tcp", "http", "80/tcp/udp"} {
		t.Run(raw, func(t *testing.T) {
			mappings, wantErr := nat.ParsePortSpec(raw)
			got, err := normalizeExposedPorts([]string{raw})
			if (err != nil) != (wantErr != nil) {
				t.Fatalf("error=%v native=%v", err, wantErr)
			}
			if err != nil {
				return
			}
			want := make([]string, 0, len(mappings))
			for _, mapping := range mappings {
				want = append(want, string(mapping.Port))
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("ports=%v native=%v", got, want)
			}
		})
	}
	got, err := normalizeExposedPorts([]string{"83/UDP", "80-82", "83/UDP"})
	want := []string{"83/udp", "80/tcp", "81/tcp", "82/tcp", "83/udp"}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("ordered ports=%v error=%v", got, err)
	}
}

func TestRemoteAddPinnedNativeMetadataDifferential(t *testing.T) {
	requireLiveInstructionCache(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	root := t.TempDir()
	lease, err := acquireStore(cacheTestStore(root))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := lease.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	}()
	network, err := newNetworkInterface(lease.store)
	if err != nil {
		t.Fatal(err)
	}
	stamp := time.Unix(1_700_000_000, 0)
	for _, tc := range []struct {
		name, header, source string
		timestamp            *time.Time
	}{
		{name: "absent", source: "/payload"},
		{name: "valid", source: "/payload", header: "Tue, 02 Jan 2024 03:04:05 GMT"},
		{name: "malformed", source: "/payload", header: "yesterday"},
		{name: "obsolete HTTP date", source: "/payload", header: "Sunday, 06-Nov-94 08:49:37 GMT"},
		{name: "override malformed", source: "/payload", header: "yesterday", timestamp: &stamp},
		{name: "root URL", source: "/"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Last-Modified", tc.header)
				_, _ = w.Write([]byte("payload"))
			}))
			defer server.Close()
			var nativeErr error
			var nativeTime time.Time
			var nativeMode os.FileMode
			for _, native := range []bool{true, false} {
				builder, err := upstream.NewBuilder(ctx, lease.store, upstream.BuilderOptions{FromImage: "scratch", PullPolicy: define.PullNever, Isolation: define.IsolationChroot, Format: define.OCIv1ImageManifest, NetworkInterface: network, SystemContext: &types.SystemContext{BigFilesTemporaryDir: root}})
				if err != nil {
					t.Fatal(err)
				}
				defer func() {
					if err := builder.Delete(); err != nil {
						t.Errorf("delete builder: %v", err)
					}
				}()
				options := upstream.AddAndCopyOptions{Timestamp: tc.timestamp}
				if native {
					err = builder.Add("/result", false, options, server.URL+tc.source)
					nativeErr = err
				} else {
					err = applyRemoteAddSource(ctx, nativeBuilder{Builder: builder}, Add{Destination: "/result"}, options, server.URL+tc.source)
					if (err != nil) != (nativeErr != nil) {
						t.Fatalf("adapter error=%v native=%v", err, nativeErr)
					}
				}
				if err != nil {
					continue
				}
				mount, err := builder.Mount(builder.MountLabel)
				if err != nil {
					t.Fatal(err)
				}
				info, statErr := os.Stat(filepath.Join(mount, "result"))
				unmountErr := builder.Unmount()
				if statErr != nil || unmountErr != nil {
					t.Fatalf("stat=%v unmount=%v", statErr, unmountErr)
				}
				if native {
					nativeTime = info.ModTime()
					nativeMode = info.Mode()
				} else if !info.ModTime().Equal(nativeTime) || info.Mode() != nativeMode {
					t.Fatalf("adapter metadata=%v %v native=%v %v", info.ModTime(), info.Mode(), nativeTime, nativeMode)
				}
			}
		})
	}
}

func TestRemoteAddDownloadCancellation(t *testing.T) {
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(started); <-r.Context().Done() }))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	builder := &inspectingAddBuilder{inspect: func(_ string, _ bool, _ upstream.AddAndCopyOptions, _ ...string) error {
		return errors.New("canceled download reached native Add")
	}}
	go func() {
		result <- applyRemoteAddSource(ctx, builder, Add{Destination: "/payload"}, upstream.AddAndCopyOptions{}, server.URL+"/payload")
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("download did not start")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation error=%v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("canceled download did not return")
	}
}
