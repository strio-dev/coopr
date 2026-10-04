# CLI

Use `coopr --help` or `coopr COMMAND --help` for all flags in the installed binary. `coopr --version` prints its version. Image commands use the [selected image store](configuration.md); components use their separate OCI store.

| Command | Purpose |
| --- | --- |
| `build [file\|context]` | Build an image. Context operands require `--file FILE`. |
| `component build [file\|context]` | Build a component. Context operands require `--file FILE`. |
| `copy SOURCE DESTINATION` | Copy a stored image or explicit engine source. |
| `component copy SOURCE DESTINATION` | Retag/export a component. |
| `image ls`, `images` | List locally named images. |
| `image inspect NAME`, `image rm NAME...` | Inspect images or remove local names. |
| `component ls`, `components` | List locally named components. |
| `component inspect REF`, `component rm NAME...` | Inspect components or remove names. |
| `system df`, `system prune` | Inspect/prune aggregate local state. |
| `cache df`, `cache prune` | Inspect/prune instruction-cache aliases and images. |

## Build controls

These controls apply to both build commands unless noted. Refer to the [execution reference](execution.md) for scope and cache rules.

| Option | Behavior |
| --- | --- |
| `--file`, `--context`, `--from` | Select definition/context or replace the first FROM image. Positional FILE uses its directory as context; `--file FILE` defaults to the current directory. |
| `--target` | Select a named output; default is the final stage. Component invocation cannot change the published target. |
| `--build-arg NAME[=VALUE]`, `--build-arg-file PATH` | Repeatable arguments/files; explicit arguments override files. Bare names import host values when set. |
| `--build-context NAME=VALUE`, `--ignorefile PATH` | Supply named inputs or override automatic ignore-file selection. |
| `--tag`, `--push` | Repeatable local names/transfer destinations; `--push` publishes the tagged registry result. |
| `--platform` | Repeated/comma-separated Linux platforms; defaults to host platform. |
| `--jobs` | Total stage/platform concurrency; default 1, zero unlimited. |
| `--pull-policy` | Image inputs: `missing` (default), `always`, `newer`, `never`. Bare `--pull` means always. |
| `--no-cache`, `--cache-ttl` | Bypass result reads or limit wall-clock publication age. Fresh results are still written. |
| `--cache`, `--cache-from`, `--cache-to` | Read/write, read-only, or write-only caches; repeatable `oci-layout:PATH` or `registry:HOST/REPOSITORY`. |
| `--network`, `--add-host` | Default RUN network and repeatable host mappings. Authored RUN network values override the default. |
| `--secret`, `--ssh`, `--allow` | Supply credentials or authorize elevated RUN behavior; see [security](../guides/security.md). |
| `--source-date-epoch`, `--timestamp`, `--rewrite-timestamp` | Set creation time, force new-layer file times, or clamp newer times. Timestamp conflicts with the other two. |
| `--metadata-file` | Write result/platform/destination metadata. |
| `--quiet`, `--logfile`, `--logsplit` | Suppress progress or record it, optionally per platform. |

Image builds also expose `--format oci|docker`, filesystem `--output`, `--iidfile`, metadata controls, `--all-platforms`, `--manifest`, signing, SBOM scanners, and confidential workload conversion. Some require native runtime capabilities or external scanner images.

### Named inputs

`--build-context NAME=VALUE` accepts local directories, `docker-image://REFERENCE`, selected `oci-layout://PATH:TAG`, public HTTP(S) tar archives, and HTTP(S)/Git/SSH/SCP-style Git repositories.

Git query options are `branch`, `tag`, or `ref`; optional `subdir` and `checksum`/`commit`; `submodules=false`; `keep-git-dir=true`; `mtime=commit`; and `fetch-by-commit=true`. The last requires a full lowercase commit SHA in `checksum`. Nested submodules fetch by default. Legacy `#ref:subdir` remains accepted. These query controls belong to named contexts; ADD retains its instruction-level Git options.

## Destinations

| Destination | Images | Components |
| --- | --- | --- |
| Plain `NAME` or `local:NAME` | Local tag. | Local tag. |
| `registry:HOST/REPOSITORY:TAG` | Registry image/index. | Registry artifact/index. |
| `oci-archive:PATH` | OCI archive. | OCI archive. |
| `podman:NAME`, `docker:NAME` | Explicit engine transfer. | Unsupported. |

Bare digests select immutable local artifacts. Invoke a local component tag with `local:NAME`; registry references do not search the local component store. See [storage](../guides/storage.md) for platform selection and digest preservation. A failed post-build transfer reports the retained result; retry with `coopr copy` or `coopr component copy`.

## Configuration and lifecycle

Global storage/runtime settings live in [configuration](configuration.md). Prune supports `--dry-run` and protects named/in-use roots. `--layers=false` reruns filesystem instructions into one new layer; `--rm` and `--force-rm` default to true. Resource controls depend on host cgroup and namespace permissions.

Global `--log-level` controls diagnostic stderr logs: `trace`, `debug`, `info`, `warn` (or `warning`), `error`, `fatal`, `panic`. Default `warn` shows warnings/errors.

Build progress goes to stderr; the final reference/digest goes to stdout. Progress includes `STEP i/n`, checkpoint IDs, `--> Using cache`, numbered stages, platform/component prefixes, final `COMMIT`, and `Successfully tagged` for named image outputs. With `--output type=tar,dest=-`, stdout carries the archive and stderr carries the final result. `--quiet` suppresses progress; `--logfile` still records it.
