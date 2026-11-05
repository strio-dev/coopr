# Caching

Coopr reuses instruction results automatically, including work inside components. Share those results through a registry or a portable local directory. Use RUN cache mounts separately to keep disposable tool data between builds.

## Reuse local results

From a repository checkout, build the [hello-world example](../tutorials/first-image.md) twice:

```sh
coopr build examples/first-image/image.coopr --tag hello:cached
coopr build examples/first-image/image.coopr --tag hello:cached
```

The second build reports `Using cache` for reused checkpoints. Coopr matches operation definitions, their dependencies, arguments, platform, execution settings, and declared sources. Changed inputs can invalidate a result and the instructions that depend on it. Stored counters count successful artifact writes per destination; a warm run can reuse execution and still update explicitly writable caches.

Local instruction snapshots live in the [native image store](storage.md). They work with the default `--layers=true`; `--layers=false` reruns filesystem instructions instead of reading those snapshots. See [layer and cache behavior](../reference/execution.md#layers-storage-and-copying).

## Share results through a registry

Use a cache repository that your machines or CI jobs can access. Replace `registry.example.com/team/coopr-cache` below and authenticate with `coopr login` when required:

```sh
coopr build examples/first-image/image.coopr --tag hello:cached \
  --cache-from registry.example.com/team/coopr-cache \
  --cache-to registry.example.com/team/coopr-cache
```

The same options apply when packaging a component:

```sh
coopr component build examples/reusable-components/components/settings/component.coopr \
  --cache-from registry.example.com/team/coopr-cache \
  --cache-to registry.example.com/team/coopr-cache
```

Both commands run from the repository checkout. `--cache-from` reads results and `--cache-to` writes them; supply both to use a repository in both directions. Each option is repeatable. Registry values name repositories without tags or digests.

## Use a portable local cache

Write an OCI cache directory that you can retain between CI jobs or move to another machine:

```sh
coopr build examples/first-image/image.coopr --tag hello:cached \
  --cache-from type=local,src=.coopr-cache \
  --cache-to type=local,dest=.coopr-cache
```

Paths are relative to the command’s working directory. Coopr creates the directory if it does not exist. Read and write destinations can differ: for example, read a registry cache and write a local cache. The contents use Coopr’s cache format and cannot be exchanged with BuildKit caches.

## What is reused

Portable caches store eligible instruction snapshots, component outputs, and package results. Restored components retain their selected image’s layers, configuration, and root metadata. RUN cache-mount contents remain local and are **not uploaded** by `--cache-to` or restored by `--cache-from`.

Portable instruction results include device-backed RUNs and remote ADD. HTTP sources are measured and Git refs resolved before an unpinned ADD can reuse a result. Dynamic bases and nested components still resolve their dependencies while reusing individual instructions. See [cache scopes](../reference/execution.md#cache-scopes) for whole-component, package and layer-group behavior.

Coopr embeds BuildKit's generic dependency solver and cache manager, while Buildah executes operations and stores images. It runs inside the Coopr process; no BuildKit daemon or worker service is required. Eligible instruction keys describe operations and dependencies rather than the creation timestamps of newly generated parent layers. Source and opaque execution boundaries still identify their actual filesystem state.

A logical key can retain several complete physical results. If a forced build creates another COPY result, an ordinary build can still select a previously cached downstream image with its original ancestors. Coopr restores that complete image and reapplies current authored metadata. It does not attach a cached layer to an unrelated parent. Ordinary COPY retains Buildah's timestamp behavior. Different physical layer counts and completed stage-worker boundaries can conservatively limit reuse.

Portable artifacts carry BuildKit's dependency graph encoding alongside Coopr result metadata and complete image graphs. Coopr's transport, authentication and integrity checks remain in use. These artifacts are Coopr caches, not a promise of interchange with native BuildKit or Dagger caches. See the [cache adapter design](../development/buildkit-cache.md).

Eligible plan invocations import only selected cache results into the native image store. Unselected historical variants remain available for later cache publication without native unpacking. Isolated parallel stage workers retain eager imports to preserve their existing ownership/relay behavior. All artifact blobs are still fetched and verified; fewer native imports do not imply fewer registry bytes or an elapsed-time speedup. Buildah and the shared Podman image store remain the execution/storage backend.

Trust cache repositories as you would other build inputs. Intermediate results may contain files removed from the final image.

## Refresh deliberately

Force instructions to execute again, or limit how old a reusable result may be:

```sh
coopr build examples/first-image/image.coopr --no-cache
coopr build examples/first-image/image.coopr --cache-ttl 24h
```

`--no-cache` forces instruction execution and saves fresh results. Portable graph records may still be imported to preserve earlier alternatives when updating a shared cache; these records cannot satisfy forced execution. It does not clear cache mounts or refresh a base image; use `--pull` to refresh registry image inputs. `--cache-ttl` limits reuse by cache-record age, not image timestamps. `--cache-ttl 0` disables result reuse while saving fresh results.

Network responses, credential values, cache-mount contents, and mutable host-volume/device contents do not invalidate conventional results. Secret and SSH declarations do enter cache identity, but their contents do not. Use `--no-cache` or change a consumed build argument when these inputs must force a rebuild. A cache hit is not a reproducibility guarantee, and required entitlements are checked even on a hit.

Writable primary-context bind mounts execute each time so their context changes remain visible to later instructions. Components using caller-context binds also bypass whole-component reuse. See [RUN mounts](../reference/execution.md#run-mounts) for context lifetime and measurement.

Sources may still need resolution before a cache lookup, so a hit does not guarantee an offline build. [Output compression](../reference/execution.md#output-compression) controls final image export; graph-cache snapshots preserve their selected immutable manifests.

## Cache mounts

Cache mounts help when a RUN must execute again: a package manager can reuse downloads even when its instruction result no longer matches.

Save this definition as `image.coopr` in a new directory:

```kdl
from "docker.io/redhat/ubi9:latest" {
    // Keep DNF downloads outside the image's filesystem layers.
    run {
        script """
            dnf install -y jq \\
                --setopt=cachedir=/var/cache/dnf \\
                --setopt=keepcache=True
            """
        mount "cache" target="/var/cache/dnf" id="ubi9-dnf" sharing="locked"
    }

    cmd {
        exec "jq" "--version"
    }
}
```

Build and run it from that directory:

```sh
coopr build image.coopr --tag tools:cached
podman run --rm localhost/tools:cached
```

DNF installs `jq` into the image while its repository metadata and downloaded RPMs stay in the mount. [`keepcache=True`](https://dnf.readthedocs.io/en/latest/conf_ref.html#keepcache-label) retains those RPMs after installation. A later RUN can reuse this data even with `--no-cache`; a new machine starts with an empty mount.

The explicit `id` shares tool data with other mounts using that ID. Omit it for an automatically scoped name that can change with the definition, platform, or component arguments. `sharing="locked"` serializes access; `shared` permits concurrent access. The bundled Buildah backend does not support `sharing="private"`; use distinct cache IDs when callers need separate caches.

Mount data is disposable: the build must work when it is empty. See [mount options](../reference/definition.md#run-mount-options) and [cache cleanup](storage.md#clear-run-cache-mounts).
