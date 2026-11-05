package buildah

import (
	digest "github.com/opencontainers/go-digest"
	upstreamdefine "go.podman.io/buildah/define"
)

// Removing generated parent layers from an operation key must not remove the
// backend compatibility boundary used by portable caches. Independently changed
// storage/image modules and graph-driver options can alter execution and import.
func instructionGraphOperationKeyWithExecutor(executor *graphExecutor, input instructionCacheInput) (digest.Digest, bool, error) {
	key, eligible, err := instructionGraphOperationKey(input)
	if err != nil || !eligible {
		return key, eligible, err
	}
	modules, err := cacheModuleVersions()
	if err != nil {
		return "", false, err
	}
	runtime := executor.instructionRuntime
	if runtime == "" {
		runtime = "none"
	}
	namespace, err := componentCacheExecutorSemantics(upstreamdefine.Version, modules, executor.isolation.String(), executor.store.GraphDriverName(), executor.store.GraphOptions(), runtime, executor.options.Output.Format)
	if err != nil {
		return "", false, err
	}
	return instructionGraphNamespacedKey(key, namespace), true, nil
}

func instructionGraphNamespacedKey(key digest.Digest, namespace string) digest.Digest {
	return digest.FromString(instructionSolverVersion + "\n" + namespace + "\n" + key.String())
}
