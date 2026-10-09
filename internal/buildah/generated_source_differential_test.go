package buildah

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"go.podman.io/buildah/define"
	"go.podman.io/buildah/imagebuildah"
	"go.podman.io/image/v5/types"
	"go.podman.io/storage"
	"go.podman.io/storage/pkg/reexec"
	"go.podman.io/storage/pkg/unshare"
)

const upstreamGeneratedWorker = "coopr-test-upstream-generated"

func init() {
	for _, name := range []string{upstreamGeneratedWorker, upstreamGeneratedWorker + "-in-a-user-namespace"} {
		reexec.Register(name, func() {
			unshare.MaybeReexecUsingUserNamespace(false)
			if err := buildUpstreamGenerated(context.Background(), os.Args[1], os.Args[2], os.Args[3], os.Args[4] == "true", os.Args[5], os.Args[6] == "true"); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			os.Exit(0)
		})
	}
}

// This test-only worker executes the pinned upstream frontend in the same
// rootless namespace setup as Coopr's workers; product builds never generate
// or parse a Containerfile.
func buildUpstreamGenerated(ctx context.Context, root, contextDir, output string, explicitAfter bool, sourcePolicyFile string, namedContext bool) (retErr error) {
	store, err := storage.GetStore(storage.StoreOptions{GraphDriverName: "vfs", GraphRoot: filepath.Join(root, "graph"), RunRoot: filepath.Join(root, "run")})
	if err != nil {
		return err
	}
	defer func() { _, err := store.Shutdown(false); retErr = errors.Join(retErr, err) }()
	file := filepath.Join(root, "Containerfile")
	contents := `FROM oci:input:latest AS producer
RUN --mount=type=bind,target=/ctx,rw cp -a /ctx/input /ctx/out
FROM --after=producer oci:out:latest
ENV generated=yes
`
	if !explicitAfter {
		contents = strings.ReplaceAll(contents, "--after=producer ", "")
	}
	if sourcePolicyFile != "" {
		contents = strings.ReplaceAll(contents, "FROM oci:out:latest", "FROM registry.invalid/generated:latest")
	}
	var contexts map[string]*define.AdditionalBuildContext
	if namedContext {
		contents = strings.ReplaceAll(contents, "FROM registry.invalid/generated:latest", "FROM generated")
		contexts = map[string]*define.AdditionalBuildContext{"generated": {IsImage: true, Value: "registry.invalid/generated:latest"}}
	}
	if err := os.WriteFile(file, []byte(contents), 0600); err != nil {
		return err
	}
	jobs := 2
	if !explicitAfter {
		jobs = 1
	}
	_, _, err = imagebuildah.BuildDockerfiles(ctx, store, define.BuildOptions{
		AdditionalBuildContexts: contexts, SourcePolicyFile: sourcePolicyFile, ContextDirectory: contextDir, Output: "oci:" + output, Layers: explicitAfter, Jobs: &jobs, SkipUnusedStages: types.NewOptionalBool(explicitAfter),
		Isolation: define.IsolationOCIRootless, Runtime: "crun", CommonBuildOpts: &define.CommonBuildOptions{},
		Compression: define.Uncompressed, SystemContext: &types.SystemContext{SignaturePolicyPath: filepath.Join(filepath.Dir(root), "policy.json")},
		Out: io.Discard, Err: os.Stderr, RemoveIntermediateCtrs: true, ForceRmIntermediateCtrs: true,
	}, file)
	return err
}
