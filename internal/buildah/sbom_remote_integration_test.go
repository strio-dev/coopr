package buildah

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/coopr/internal/oci"
	"go.podman.io/buildah/define"
	"go.podman.io/image/v5/types"
	"go.podman.io/storage"
	"oras.land/oras-go/v2"
	orasoci "oras.land/oras-go/v2/content/oci"
	"oras.land/oras-go/v2/registry/remote"
)

func TestSBOMScannerMaterializesRemoteImage(t *testing.T) {
	requireLiveInstructionCache(t)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	reference, _ := newLiveBusyBoxRegistry(t, ctx)
	platform := v1.Platform{OS: "linux", Architecture: runtime.GOARCH}
	for _, policy := range []string{"missing", "always", "never"} {
		t.Run(policy, func(t *testing.T) {
			root := t.TempDir()
			options := remoteSBOMTestOptions(t, root, reference)
			resolver, err := oci.NewResolver(oci.Options{
				NativeStore: NativeStoreOptions(options.Store), PullPolicy: policy,
				TLSVerify: new(false), SignaturePolicyPath: options.SystemContext.SignaturePolicyPath,
			})
			if err != nil {
				t.Fatal(err)
			}
			options.Resolver = resolver
			_, err = BuildPlan(ctx, testPlan(t, `from "scratch" {}`), options)
			if policy == "never" {
				if err == nil || !strings.Contains(err.Error(), "pull policy is never") {
					t.Fatalf("missing scanner with pull-never: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if data, err := os.ReadFile(options.Output.SBOM[0].SBOMOutput); err != nil || !strings.Contains(string(data), "CycloneDX") {
				t.Fatalf("remote scanner output = %q, %v", data, err)
			}
			if _, found, err := nativeFixtureSelection(ctx, options.Store, reference, platform); err != nil || !found {
				t.Fatalf("scanner was not retained in native storage: found=%t, %v", found, err)
			}
		})
	}
}

func TestSBOMScannerMaterializesSelectedImageAfterTagMoves(t *testing.T) {
	requireLiveInstructionCache(t)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	_, authority := newLiveBusyBoxRegistry(t, ctx)
	reference := authority + "/coopr/busybox:latest"
	root := t.TempDir()
	options := remoteSBOMTestOptions(t, root, reference)
	resolver, err := oci.NewResolver(oci.Options{
		NativeStore: NativeStoreOptions(options.Store), PullPolicy: "always",
		TLSVerify: new(false), SignaturePolicyPath: options.SystemContext.SignaturePolicyPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	options.Resolver = resolver
	platform := v1.Platform{OS: "linux", Architecture: runtime.GOARCH}
	selected, err := SelectImageSource(ctx, resolver, reference, platform)
	if err != nil || !selected.Remote {
		t.Fatalf("select remote scanner: %+v, %v", selected, err)
	}
	lease, err := acquireStore(options.Store)
	if err != nil {
		t.Fatal(err)
	}
	_, absentErr := lease.store.Image(selected.ImageID)
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(absentErr, storage.ErrImageUnknown) {
		t.Fatalf("selected scanner was already materialized: %v", absentErr)
	}
	// Move the tag to a valid image without the scanner executable. A fresh
	// resolution would select a different config and fail to execute the scan.
	replacement, err := orasoci.NewWithContext(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	manifest, config := sourceTestImage(t, ctx, replacement, platform, "replacement")
	repository, err := remote.NewRepository(authority + "/coopr/busybox")
	if err != nil {
		t.Fatal(err)
	}
	repository.PlainHTTP = true
	if _, err := oras.Copy(ctx, replacement, manifest.Digest.String(), repository, "latest", oras.DefaultCopyOptions); err != nil {
		t.Fatal(err)
	}
	options.ResolvedBases = map[ResolvedBaseKey]ResolvedImageSource{
		{Reference: reference, Platform: "linux/" + runtime.GOARCH}: selected,
	}
	if _, err := BuildPlan(ctx, testPlan(t, `from "scratch" {}`), options); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(options.Output.SBOM[0].SBOMOutput); err != nil || !strings.Contains(string(data), "CycloneDX") {
		t.Fatalf("selected scanner output = %q, %v", data, err)
	}
	stored, found, err := nativeFixtureSelection(ctx, options.Store, selected.ImageID, platform)
	if err != nil || !found || stored.ImageID != selected.ImageID {
		t.Fatalf("materialized scanner = %+v, found=%t, %v; want %s", stored, found, err, selected.ImageID)
	}
	if _, found, err := nativeFixtureSelection(ctx, options.Store, config.Digest.Encoded(), platform); err != nil || found {
		t.Fatalf("replacement scanner was imported: found=%t, %v", found, err)
	}
}

func remoteSBOMTestOptions(t *testing.T, root, reference string) PlanOptions {
	t.Helper()
	return PlanOptions{
		Store: cacheTestStore(root), ContextDir: root, Isolation: "rootless", Runtime: "crun",
		SystemContext: &types.SystemContext{SignaturePolicyPath: writeComponentTestPolicy(t, root), BigFilesTemporaryDir: root},
		Output: Output{Path: filepath.Join(root, "layout"), SBOM: []define.SBOMScanOptions{{
			Image:         reference,
			Commands:      []string{`/bin/busybox sh -c 'test -d {ROOTFS} && printf "{\"bomFormat\":\"CycloneDX\",\"components\":[]}" > {OUTPUT}'`},
			MergeStrategy: define.SBOMMergeStrategyCycloneDXByComponentNameAndVersion,
			SBOMOutput:    filepath.Join(root, "sbom.json"),
		}}},
	}
}
