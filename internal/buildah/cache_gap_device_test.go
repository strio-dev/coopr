package buildah

import (
	"bytes"
	"context"
	"coopr/internal/definition"
	"coopr/internal/oci"
	"coopr/internal/planner"
	"fmt"
	commonconfig "go.podman.io/common/pkg/config"
	imageoci "go.podman.io/image/v5/oci/layout"
	"go.podman.io/image/v5/types"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	upstream "go.podman.io/buildah"
	"go.podman.io/buildah/define"
	"go.podman.io/buildah/imagebuildah"
)

func TestCacheGapNativeDeviceCacheComparator(t *testing.T) {
	requireLiveInstructionCache(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	root := t.TempDir()
	store := cacheTestStore(root)
	base := newLiveBusyBoxStorage(t, ctx, root, store)
	specDir := filepath.Join(root, "cdi")
	if err := os.Mkdir(specDir, 0o700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, "Containerfile")
	if err := os.WriteFile(file, []byte(fmt.Sprintf("FROM %s\nRUN --network=none printf '%%s' \"$COOPR_DEVICE_VALUE\" >/proof\n", base.reference)), 0o600); err != nil {
		t.Fatal(err)
	}
	lease, err := acquireStore(store)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := lease.Close(); err != nil {
			t.Error(err)
		}
	}()
	for _, attempt := range []struct {
		name, value, want string
		noCache           bool
	}{
		{"cold", "first", "first", false}, {"warm-changed-spec", "second", "first", false}, {"refresh", "second", "second", true},
	} {
		spec := fmt.Sprintf(`{"cdiVersion":"0.6.0","kind":"test.example/env","devices":[{"name":"proof","containerEdits":{"env":["COOPR_DEVICE_VALUE=%s"]}}]}`, attempt.value)
		if err := os.WriteFile(filepath.Join(specDir, "env.json"), []byte(spec), 0o600); err != nil {
			t.Fatal(err)
		}
		var progress bytes.Buffer
		layout := filepath.Join(root, attempt.name)
		id, _, err := imagebuildah.BuildDockerfiles(ctx, lease.store, define.BuildOptions{
			ContextDirectory: root, Output: "native-device-cache:test", OutputFormat: define.OCIv1ImageManifest, Layers: true, NoCache: attempt.noCache, PullPolicy: define.PullNever, Isolation: define.IsolationOCIRootless, Runtime: "crun", CommonBuildOpts: &define.CommonBuildOptions{}, Compression: define.Uncompressed, Out: &progress, Err: &progress, RemoveIntermediateCtrs: true, ForceRmIntermediateCtrs: true, Devices: []string{"test.example/env=proof"}, CDIConfigDir: specDir,
		}, file)
		if err != nil {
			t.Fatalf("%s native device build: %v\n%s", attempt.name, err, progress.String())
		}
		destination, err := imageoci.NewReference(layout, "latest")
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := upstream.Push(ctx, id, destination, upstream.PushOptions{Store: lease.store, SignaturePolicyPath: writeComponentTestPolicy(t, root)}); err != nil {
			t.Fatal(err)
		}
		if got := localComponentLastFile(t, layout, "proof"); got != attempt.want {
			t.Fatalf("%s native device cache output=%q want=%q\n%s", attempt.name, got, attempt.want, progress.String())
		}
		if attempt.name == "warm-changed-spec" && !strings.Contains(progress.String(), "Using cache") {
			t.Fatalf("native warm build lacked cache hit: %s", progress.String())
		}
		t.Logf("%s native device result=%q cache=%t", attempt.name, attempt.want, strings.Contains(progress.String(), "Using cache"))
	}
}

func TestCacheGapDeviceSelectionAndAuthorizationAcrossCacheScopes(t *testing.T) {
	requireLiveInstructionCache(t)
	for _, scope := range []string{"instruction", "component", "package", "global"} {
		t.Run(scope, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
			defer cancel()
			root := t.TempDir()
			base, _ := newLiveBusyBoxRegistry(t, ctx)
			specDir := filepath.Join(root, "cdi")
			if err := os.Mkdir(specDir, 0o700); err != nil {
				t.Fatal(err)
			}
			specPath := filepath.Join(specDir, "env.json")
			spec := `{"cdiVersion":"0.6.0","kind":"test.example/env","devices":[{"name":"alpha","containerEdits":{"env":["COOPR_DEVICE_VALUE=alpha"]}},{"name":"beta","containerEdits":{"env":["COOPR_DEVICE_VALUE=beta"]}}]}`
			if err := os.WriteFile(specPath, []byte(spec), 0o600); err != nil {
				t.Fatal(err)
			}
			run := `run "printf '%s' \"$COOPR_DEVICE_VALUE\" >/proof" network="none" { device "accelerator,required"; }`
			if scope == "global" {
				run = `run "printf '%s' \"$COOPR_DEVICE_VALUE\" >/proof" network="none"`
			}
			resolver := localConfigComponentResolver(t, ctx, root, "extend { "+run+"; }", false)
			resolver, err := oci.NewResolver(oci.Options{ComponentStoreDir: resolver.ComponentStoreDir(), TLSVerify: new(false)})
			if err != nil {
				t.Fatal(err)
			}
			policy := writeComponentTestPolicy(t, root)
			cacheDir := filepath.Join(root, "cache")
			build := func(name, selected string, authorized bool) (bool, error) {
				dir := filepath.Join(root, name)
				store := cacheTestStore(dir)
				controls := RunControls{CDISpecDirs: []string{specDir}}
				if scope == "global" {
					controls.Devices = []string{"test.example/env=" + selected}
				}
				allow := []string{"device=test.example/env=" + selected + ",alias=accelerator"}
				if !authorized {
					allow = nil
				}
				if scope == "package" {
					plan := testPublicationPlan(t, fmt.Sprintf(`from %q as="producer" { %s; }
package as="bundle" { copy "/proof" "/proof" from="producer"; }
extend { copy "/proof" "/proof" from="bundle"; }`, base, run))
					path := filepath.Join(dir, "bundle.tar")
					_, err := PublishPlan(ctx, plan, PublicationOptions{PlanOptions: PlanOptions{Store: store, ContextDir: root, Resolver: resolver, CacheLocalDir: cacheDir, Isolation: "rootless", Runtime: "crun", Allow: allow, RunControls: controls, SystemContext: &types.SystemContext{SignaturePolicyPath: policy}}, PackagePaths: map[string]string{"bundle": path}})
					if err != nil {
						return false, err
					}
					assertPackageTarFile(t, path, "proof", selected)
					lease, err := acquireStore(store)
					if err != nil {
						return false, err
					}
					images, imageErr := lease.store.Images()
					closeErr := lease.Close()
					if imageErr != nil {
						return false, imageErr
					}
					if closeErr != nil {
						return false, closeErr
					}
					return len(images) == 0, nil
				}
				body := run
				if scope == "component" {
					body = `component "config"`
				}
				result, err := BuildPlanSupervised(ctx, testPlan(t, fmt.Sprintf("from %q { %s; }", base, body)), SupervisedPlanOptions{Store: store, ContextDir: root, ComponentStoreDir: resolver.ComponentStoreDir(), CacheLocalDir: cacheDir, Isolation: "rootless", Runtime: "crun", Allow: allow, RunControls: controls, TLSVerify: new(false), SignaturePolicyPath: policy, Output: Output{Path: filepath.Join(dir, "layout")}, Stdout: io.Discard, Stderr: io.Discard})
				if err != nil {
					return false, err
				}
				if got := localComponentLastFile(t, result.Layout, "proof"); got != selected {
					t.Fatalf("%s device output=%q want=%q", name, got, selected)
				}
				return result.CacheStats.Hits == 1 && result.CacheStats.Misses == 0, nil
			}
			for _, attempt := range []struct {
				name, device string
				hit          bool
			}{{"alpha-cold", "alpha", false}, {"alpha-warm", "alpha", true}, {"beta-cold", "beta", false}, {"beta-warm", "beta", true}} {
				hit, err := build(attempt.name, attempt.device, true)
				if err != nil {
					t.Fatal(err)
				}
				if hit != attempt.hit {
					t.Fatalf("%s cache hit=%t expected=%t", attempt.name, hit, attempt.hit)
				}
			}
			if scope != "global" {
				if _, err := build("denied", "beta", false); err == nil || (!strings.Contains(err.Error(), "requires --allow device") && !strings.Contains(err.Error(), "required device")) {
					t.Fatalf("warm unauthorized device=%v", err)
				}
			}
			if err := os.Remove(specPath); err != nil {
				t.Fatal(err)
			}
			if _, err := build("missing-required", "beta", true); err == nil || !strings.Contains(err.Error(), "required device") {
				t.Fatalf("warm missing-required device=%v", err)
			}
		})
	}
}

func TestCacheGapOptionalDeviceAvailabilityInvalidatesPortableCache(t *testing.T) {
	requireLiveInstructionCache(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	root := t.TempDir()
	base, _ := newLiveBusyBoxRegistry(t, ctx)
	specDir := filepath.Join(root, "cdi")
	if err := os.Mkdir(specDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, attempt := range []struct {
		name, want string
		hit        bool
	}{{"missing", "none", false}, {"missing-warm", "none", true}, {"present", "present", false}, {"present-warm", "present", true}} {
		if attempt.name == "present" {
			if err := os.WriteFile(filepath.Join(specDir, "env.json"), []byte(`{"cdiVersion":"0.6.0","kind":"test.example/env","devices":[{"name":"optional","containerEdits":{"env":["COOPR_DEVICE_VALUE=present"]}}]}`), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		dir := filepath.Join(root, attempt.name)
		result, err := BuildPlanSupervised(ctx, testPlan(t, fmt.Sprintf(`from %q { run "printf '%%s' \"${COOPR_DEVICE_VALUE:-none}\" >/proof" network="none" { device "test.example/env=optional"; } }`, base)), SupervisedPlanOptions{Store: cacheTestStore(dir), ContextDir: root, CacheLocalDir: filepath.Join(root, "cache"), Isolation: "rootless", Runtime: "crun", Allow: []string{"device"}, RunControls: RunControls{CDISpecDirs: []string{specDir}}, TLSVerify: new(false), SignaturePolicyPath: writeComponentTestPolicy(t, root), Output: Output{Path: filepath.Join(dir, "layout")}, Stdout: io.Discard, Stderr: io.Discard})
		if err != nil {
			t.Fatal(err)
		}
		if got := localComponentLastFile(t, result.Layout, "proof"); got != attempt.want {
			t.Fatalf("%s output=%q", attempt.name, got)
		}
		hit := result.CacheStats.Hits == 1 && result.CacheStats.Misses == 0
		if hit != attempt.hit {
			t.Fatalf("%s stats=%+v", attempt.name, result.CacheStats)
		}
	}
}

func TestCacheGapConfiguredCDIDirectoryIgnoresBrokenDefaultSpecs(t *testing.T) {
	root := t.TempDir()
	broken, valid := filepath.Join(root, "broken"), filepath.Join(root, "valid")
	for _, dir := range []string{broken, valid} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(broken, "bad.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(valid, "good.json"), []byte(`{"cdiVersion":"0.6.0","kind":"test.example/env","devices":[{"name":"proof","containerEdits":{"env":["COOPR_DEVICE_VALUE=valid"]}}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	conf := filepath.Join(root, "containers.conf")
	if err := os.WriteFile(conf, []byte(fmt.Sprintf("[engine]\ncdi_spec_dirs=[%q]\n", broken)), 0o600); err != nil {
		t.Fatal(err)
	}
	// Restore the public process-local configuration after restoring its selector.
	t.Cleanup(func() {
		if _, err := commonconfig.Reload(); err != nil {
			t.Error(err)
		}
	})
	t.Setenv("CONTAINERS_CONF", conf)
	if _, err := commonconfig.Reload(); err != nil {
		t.Fatal(err)
	}
	operation := planner.Operation{Instruction: definition.Instruction{Name: "run", Children: []definition.Instruction{{Name: "device", Arguments: []string{"test.example/env=proof,required"}}}}}
	if _, err := runDeviceCacheForPlannedOperation(operation, RunControls{}); err == nil {
		t.Fatal("broken-default control unexpectedly loaded")
	}
	pathOnly := planner.Operation{Instruction: definition.Instruction{Name: "run", Arguments: []string{"true"}}}
	if identity, err := cacheRunDeviceIdentity(pathOnly, RunControls{Devices: []string{"/dev/null"}}, nil); err != nil || identity == "" {
		t.Fatalf("native path device depended on broken CDI defaults: identity=%q error=%v", identity, err)
	}
	cache, err := runDeviceCacheForPlannedOperation(operation, RunControls{CDISpecDirs: []string{valid}})
	if err != nil {
		t.Fatal(err)
	}
	selected, err := resolveRunDeviceSpecs(cache, []runDeviceRequest{{Name: "test.example/env=proof", Required: true}})
	if err != nil || len(selected) != 1 {
		t.Fatalf("configured CDI selection=%v error=%v", selected, err)
	}
}
