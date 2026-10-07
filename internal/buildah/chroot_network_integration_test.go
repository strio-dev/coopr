package buildah

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"coopr/internal/definition"
	"coopr/internal/oci"
	"coopr/internal/planner"
	"go.podman.io/image/v5/types"
)

// A cache produced under OCI isolation must not authorize a RUN whose network
// policy cannot be honored under chroot, even when no command would execute.
func TestChrootNetworkPolicyRejectsCachedImageAndComponent(t *testing.T) {
	requireLiveInstructionCache(t)
	for _, kind := range []string{"image", "component"} {
		t.Run(kind, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			root := t.TempDir()
			store := cacheTestStore(root)
			base := newLiveBusyBoxStorage(t, ctx, root, store)
			source := "from \"" + base.reference + "\"\n"
			run := `run "od -An -N16 -tx1 /dev/urandom | tr -d ' \\n' >/proof" network="none"` + "\n"
			if kind == "component" {
				_ = localConfigComponentResolver(t, ctx, root, "extend\n"+run, false)
				source += "component \"local:config\"\n"
			} else {
				source += run
			}
			plan := testPlan(t, source)
			options := SupervisedPlanOptions{
				Store: store, ContextDir: root, Isolation: "rootless", Runtime: "crun",
				ComponentStoreDir: filepath.Join(root, "components"), CacheLocalDir: filepath.Join(root, "cache"),
				SignaturePolicyPath: writeComponentTestPolicy(t, root), Stdout: io.Discard,
			}
			var coldProof string
			for _, name := range []string{"cold", "warm"} {
				var logs strings.Builder
				options.Stderr = &logs
				options.Output = Output{Path: filepath.Join(root, name)}
				result, err := BuildPlanSupervised(ctx, plan, options)
				if err != nil {
					t.Fatal(err)
				}
				proof := localComponentLastFile(t, options.Output.Path, "proof")
				if name == "cold" {
					coldProof = proof
				} else {
					if proof != coldProof {
						t.Fatalf("warm %s RUN executed again: cold=%q warm=%q", kind, coldProof, proof)
					}
					if kind == "component" && result.CacheStats.Hits != 1 {
						t.Fatalf("warm component did not use portable cache: %+v", result.CacheStats)
					}
					if kind == "image" && !strings.Contains(logs.String(), "--> Using cache ") {
						t.Fatalf("warm image did not use instruction cache:\n%s", logs.String())
					}
				}
			}
			options.Isolation = "chroot"
			options.Output = Output{Path: filepath.Join(root, "rejected")}
			_, err := BuildPlanSupervised(ctx, plan, options)
			assertChrootNetworkRejected(t, err, options.Output.Path)
		})
	}
}

func TestChrootNetworkPolicyRejectsCachedPackage(t *testing.T) {
	requireLiveInstructionCache(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	root := t.TempDir()
	base, _ := newLiveBusyBoxRegistry(t, ctx)
	component := parseWorkerDefinition(t, `
from "`+base+`" as="producer"
run "od -An -N16 -tx1 /dev/urandom | tr -d ' \\n' >/proof" network="none"
package as="payload"
copy "/proof" "/proof" from="producer"
extend
copy "/proof" "/proof" from="payload"
`)
	options := SupervisedPlanOptions{
		ContextDir: root, Isolation: "rootless", Runtime: "crun",
		ComponentStoreDir: filepath.Join(root, "components"), CacheLocalDir: filepath.Join(root, "cache"),
		TLSVerify: new(false), SignaturePolicyPath: writeComponentTestPolicy(t, root),
		Stdout: io.Discard, Stderr: io.Discard,
	}
	var coldDigest string
	for _, name := range []string{"cold", "warm"} {
		options.Store = cacheTestStore(filepath.Join(root, name))
		options.Output = Output{Path: filepath.Join(root, name, "component")}
		result, err := PublishDefinitionSupervised(ctx, component, planner.Options{Mode: planner.Publish}, options)
		if err != nil {
			t.Fatal(err)
		}
		if name == "cold" {
			coldDigest = result.Root.Digest.String()
		} else {
			if result.Root.Digest.String() != coldDigest {
				t.Fatal("warm package publication changed its cached component")
			}
			// A fresh native store proves this publication skipped its producer.
			lease, err := acquireStore(options.Store)
			if err != nil {
				t.Fatal(err)
			}
			images, imageErr := lease.store.Images()
			closeErr := lease.Close()
			if imageErr != nil || closeErr != nil {
				t.Fatalf("inspect warm package store: images=%v close=%v", imageErr, closeErr)
			}
			if len(images) != 0 {
				t.Fatalf("warm package cache executed producers: %d images", len(images))
			}
		}
	}
	options.Isolation = "chroot"
	options.Store = cacheTestStore(filepath.Join(root, "rejected"))
	options.Output = Output{Path: filepath.Join(root, "rejected", "component")}
	_, err := PublishDefinitionSupervised(ctx, component, planner.Options{Mode: planner.Publish}, options)
	assertChrootNetworkRejected(t, err, options.Output.Path)
}

func assertChrootNetworkRejected(t *testing.T, err error, output string) {
	t.Helper()
	const want = "RUN network=none cannot be used with chroot isolation; use --isolation=oci or --isolation=rootless"
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("cached chroot build rejection = %v, want %q", err, want)
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatalf("rejected build produced output %s: %v", output, err)
	}
}

func TestChrootNetworkPolicyRejectsLegacyInheritedOnBuild(t *testing.T) {
	requireLiveInstructionCache(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	root := t.TempDir()
	store := cacheTestStore(root)
	base := newLiveBusyBoxStorage(t, ctx, root, store)
	parentLayout := filepath.Join(root, "parent")
	parentDefinition := parseWorkerDefinition(t, "from \""+base.reference+"\"\n")
	parentDefinition.Instructions = append(parentDefinition.Instructions, definition.Instruction{Name: "onbuild", Arguments: []string{"RUN --network=none printf inherited >/proof"}})
	parentPlan, err := planner.Create(parentDefinition, planner.Options{Mode: planner.Build, Platform: runtime.GOOS + "/" + runtime.GOARCH})
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := oci.NewResolver(oci.Options{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = BuildPlan(ctx, parentPlan, PlanOptions{
		Store: store, ContextDir: root, Isolation: "rootless", Runtime: "crun",
		Output: Output{Path: parentLayout, Format: outputFormatDocker}, Resolver: resolver,
	})
	if err != nil {
		t.Fatal(err)
	}
	config, err := oci.ReadImageConfigLayout(ctx, parentLayout)
	if err != nil {
		t.Fatal(err)
	}
	selected, err := oci.LayoutRoot(parentLayout)
	if err != nil {
		t.Fatal(err)
	}
	const reference = "fixture.local/coopr/onbuild:latest"
	parentID := importTestImageToNative(t, ctx, store, parentLayout, reference, selected, &types.SystemContext{SignaturePolicyPath: writeComponentTestPolicy(t, root)})
	plan := testPlan(t, "from \""+reference+"\"\n")
	if plan.Stages[0].InheritedOnBuildPlanned {
		t.Fatal("legacy test must leave inherited ONBUILD unplanned")
	}
	options := PlanOptions{
		Store: store, ContextDir: root, Isolation: "rootless", Runtime: "crun",
		ResolvedBases: map[ResolvedBaseKey]ResolvedImageSource{
			{Reference: reference, Platform: plan.Stages[0].Platform}: {
				ImageID: parentID, Root: selected, Selected: selected, ConfigData: config, Reference: reference,
			},
		},
		Output: Output{Path: filepath.Join(root, "oci")},
	}
	if _, err := BuildPlan(ctx, plan, options); err != nil {
		t.Fatal(err)
	}
	if proof := localComponentLastFile(t, options.Output.Path, "proof"); proof != "inherited" {
		t.Fatalf("OCI inherited RUN proof = %q", proof)
	}
	options.Isolation = "chroot"
	options.Output = Output{Path: filepath.Join(root, "rejected")}
	_, err = BuildPlan(ctx, plan, options)
	assertChrootNetworkRejected(t, err, options.Output.Path)
}

func TestChrootNetworkPolicyRejectsDirectBuild(t *testing.T) {
	run := Run{Command: []string{"/bin/sh", "-c", "printf executed"}, Network: "none"}
	for _, operation := range []Operation{run, &run} {
		root := t.TempDir()
		output := filepath.Join(root, "rejected")
		_, err := Build(context.Background(), Request{
			Store: cacheTestStore(root), ContextDir: root, Isolation: "chroot",
			Operations: []Operation{operation}, Output: Output{Path: output},
		})
		assertChrootNetworkRejected(t, err, output)
	}
}
