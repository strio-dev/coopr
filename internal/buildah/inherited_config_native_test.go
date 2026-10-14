package buildah

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"coopr/internal/imageconfig"
	"coopr/internal/oci"
	"github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	upstream "go.podman.io/buildah"
	"go.podman.io/buildah/define"
	dockerspec "go.podman.io/buildah/docker"
	"go.podman.io/buildah/imagebuildah"
	"go.podman.io/image/v5/manifest"
	"maps"
	orasoci "oras.land/oras-go/v2/content/oci"
	"runtime"
	"slices"
)

func TestInheritedConfigurationAdmissionMatchesNativeBuildah(t *testing.T) {
	requireLiveInstructionCache(t)
	for _, test := range []struct {
		name      string
		configure func(*upstream.Builder)
	}{
		{"empty healthcheck", func(b *upstream.Builder) { b.SetHealthcheck(&dockerspec.HealthConfig{}) }},
		{"none interval", func(b *upstream.Builder) {
			b.SetHealthcheck(&dockerspec.HealthConfig{Test: []string{"NONE"}, Interval: time.Second})
		}},
		{"unknown test mode", func(b *upstream.Builder) { b.SetHealthcheck(&dockerspec.HealthConfig{Test: []string{"OTHER", "true"}}) }},
		{"extra shell arguments", func(b *upstream.Builder) {
			b.SetHealthcheck(&dockerspec.HealthConfig{Test: []string{"CMD-SHELL", "one", "two"}})
		}},
		{"negative interval", func(b *upstream.Builder) {
			b.SetHealthcheck(&dockerspec.HealthConfig{Test: []string{"CMD", "true"}, Interval: -time.Second})
		}},
		{"negative retries", func(b *upstream.Builder) {
			b.SetHealthcheck(&dockerspec.HealthConfig{Test: []string{"CMD", "true"}, Retries: -1})
		}},
		{"empty shell", func(b *upstream.Builder) { b.SetShell([]string{}) }},
		{"string shell", func(b *upstream.Builder) { b.SetShell([]string{}) }},
		{"empty cmd", func(b *upstream.Builder) { b.SetCmd([]string{}) }},
		{"empty entrypoint", func(b *upstream.Builder) { b.SetEntrypoint([]string{}) }},
		{"volume", func(b *upstream.Builder) { b.AddVolume("/data") }},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newLocalComponentFixture(t)
			lease, err := acquireStore(f.options.Store)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if lease != nil {
					if err := lease.Close(); err != nil {
						t.Error(err)
					}
				}
			}()
			network, err := newNetworkInterface(lease.store)
			if err != nil {
				t.Fatal(err)
			}
			b, err := upstream.NewBuilder(f.ctx, lease.store, upstream.BuilderOptions{FromImage: "scratch", PullPolicy: define.PullNever, Isolation: define.IsolationChroot, NetworkInterface: network})
			if err != nil {
				t.Fatal(err)
			}
			test.configure(b)
			id, _, _, err := b.Commit(f.ctx, nil, upstream.CommitOptions{PreferredManifestType: define.Dockerv2ImageManifest})
			if err != nil {
				t.Fatal(err)
			}
			if err := b.Delete(); err != nil {
				t.Fatal(err)
			}
			if test.name == "empty shell" || test.name == "string shell" {
				shell := json.RawMessage(`[]`)
				if test.name == "string shell" {
					shell = json.RawMessage(`"/bin/sh"`)
				}
				raw := []byte(fmt.Sprintf(`{"architecture":%q,"os":"linux","rootfs":{"type":"layers","diff_ids":[]},"history":[],"config":{"Shell":%s}}`, runtime.GOARCH, shell))
				layout := filepath.Join(f.root, "empty-shell")
				content, err := orasoci.NewWithContext(f.ctx, layout)
				if err != nil {
					t.Fatal(err)
				}
				config := descriptor(manifest.DockerV2Schema2ConfigMediaType, raw)
				if err := content.Push(f.ctx, config, bytes.NewReader(raw)); err != nil {
					t.Fatal(err)
				}
				data, err := json.Marshal(v1.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: manifest.DockerV2Schema2MediaType, Config: config})
				if err != nil {
					t.Fatal(err)
				}
				root := descriptor(manifest.DockerV2Schema2MediaType, data)
				if err := content.Push(f.ctx, root, bytes.NewReader(data)); err != nil {
					t.Fatal(err)
				}
				id, err = ImportSelectedImage(f.ctx, lease.store, nil, layout, root)
				if err != nil {
					t.Fatal(err)
				}
			}
			file := filepath.Join(f.contextDir, "Containerfile")
			if err := os.WriteFile(file, []byte(fmt.Sprintf("FROM %s\nLABEL reviewed=yes\n", id)), 0600); err != nil {
				t.Fatal(err)
			}
			nativeID, _, err := imagebuildah.BuildDockerfiles(f.ctx, lease.store, define.BuildOptions{ContextDirectory: f.contextDir, Layers: true, PullPolicy: define.PullNever, Isolation: define.IsolationChroot, OutputFormat: define.Dockerv2ImageManifest, CommonBuildOpts: &define.CommonBuildOptions{}, Out: io.Discard, Err: io.Discard, RemoveIntermediateCtrs: true, ForceRmIntermediateCtrs: true}, file)
			if err != nil {
				t.Fatal(err)
			}
			nativeRaw, err := packageImageConfig(f.ctx, lease.store, nativeID, nil)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := packageImageConfig(f.ctx, lease.store, id, nil)
			if err != nil {
				t.Fatal(err)
			}
			logical, err := imageconfig.Parse(raw)
			if err != nil {
				t.Fatalf("native inherited configuration rejected: %v", err)
			}
			if _, err := logical.MarshalJSON(); err != nil {
				t.Fatal(err)
			}
			if err := lease.Close(); err != nil {
				t.Fatal(err)
			}
			lease = nil
			source := fmt.Sprintf("from %q {label reviewed=\"yes\";}\n", id)
			for _, attempt := range []string{"cold", "warm"} {
				var progress bytes.Buffer
				layout := filepath.Join(f.root, attempt)
				result, err := BuildPlanSupervised(f.ctx, testPlan(t, source), SupervisedPlanOptions{Store: f.options.Store, ContextDir: f.contextDir, Isolation: "rootless", Runtime: "crun", Output: Output{Path: layout, Format: outputFormatDocker}, CacheLocalDir: f.options.CacheLocalDir, Stdout: io.Discard, Stderr: &progress})
				if err != nil {
					t.Fatalf("%s Coopr build: %v", attempt, err)
				}
				actualRaw, err := oci.ReadImageConfigLayout(f.ctx, layout)
				if err != nil {
					t.Fatal(err)
				}
				var actual, native struct {
					Config struct {
						Healthcheck *imageconfig.Healthcheck
						Shell       []string
						Cmd         []string
						Entrypoint  []string
						Volumes     map[string]struct{}
					}
				}
				if err := json.Unmarshal(nativeRaw, &native); err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal(actualRaw, &actual); err != nil {
					t.Fatal(err)
				}
				actualHealth, _ := json.Marshal(actual.Config.Healthcheck)
				nativeHealth, _ := json.Marshal(native.Config.Healthcheck)
				if !bytes.Equal(actualHealth, nativeHealth) || !slices.Equal(actual.Config.Shell, native.Config.Shell) || !slices.Equal(actual.Config.Cmd, native.Config.Cmd) || !slices.Equal(actual.Config.Entrypoint, native.Config.Entrypoint) || !maps.Equal(actual.Config.Volumes, native.Config.Volumes) {
					t.Fatalf("%s inherited configuration changed: got %+v native %+v", attempt, actual, native)
				}
				if result.CacheStats.Errors != 0 {
					t.Fatalf("%s cache restore errors: %+v", attempt, result.CacheStats)
				}
				t.Logf("%s cache outcomes: %+v", attempt, result.CacheStats)
			}
		})
	}
}
