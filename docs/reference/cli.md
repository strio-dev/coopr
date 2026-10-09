# CLI

Use `coopr --help` or `coopr COMMAND --help` for all flags in the installed binary. `coopr --version` prints its version. Image commands use the [native image store](configuration.md); components use their separate OCI store.

| Command | Purpose |
| --- | --- |
| `build [file\|context]` | Build an image. Context operands require `--file FILE`. |
| `component build [file\|context]` | Build a component. Context operands require `--file FILE`. |
| `copy SOURCE DESTINATION` | Copy a stored image or explicit engine source. |
| `component copy SOURCE DESTINATION` | Retag/export a component. |
| `login [REGISTRY]`, `logout [REGISTRY]` | Use the native containers authentication file and credential helpers. |
| `pull IMAGE...`, `image pull IMAGE...` | Pull images into native storage; default policy is `always`. |
| `push IMAGE [DESTINATION]`, `image push` | Publish a local image to a registry; destination defaults to its name. |
| `tag IMAGE NAME...`, `image tag` | Add local image names. |
| `save IMAGE...`, `image save` | Write a Docker archive by default, or an OCI archive with `--format oci-archive`. |
| `load`, `image load` | Import an archive from stdin or `--input FILE`. |
| `exists IMAGE`, `image exists` | Test local image presence without pulling. |
| `history IMAGE`, `image history` | Show image history, with optional platform selection. |
| `component pull REF --tag NAME` | Pull all component platforms and optionally assign a local name. |
| `component push SOURCE DESTINATION` | Publish a stored component to a registry. |
| `component tag SOURCE NAME...` | Add local component names. |
| `component save REF`, `component load` | Export/import a component OCI archive. |
| `component exists REF` | Test local component presence without pulling. |
| `manifest create`, `add`, `annotate`, `inspect`, `push`, `remove`, `rm`, `exists` | Manage native image manifest lists. |
| `info` | Show effective host, runtime, version, and storage information. |
| `image ls`, `images` | List locally named images. |
| `image inspect NAME`, `image rm NAME...` | Inspect images or remove local names. |
| `image prune` | Remove dangling images and unused instruction snapshots. |
| `component ls`, `components` | List locally named components. |
| `component inspect REF`, `component rm NAME...` | Inspect components or remove names. |
| `system df`, `system prune` | Report storage usage or prune images and unnamed component content. |

## Build controls

These controls apply to both build commands unless noted. Refer to the [execution reference](execution.md) for scope and cache rules.

| Option | Behavior |
| --- | --- |
| `--file`, `--from` | Read explicit definition files in the order supplied, or replace the first FROM image. Positional FILE uses its directory as context; `--file FILE [CONTEXT]` defaults to the current directory. |
| `--target` | Select a named output; default is the final stage. Component invocation cannot change the published target. |
| `--build-arg NAME[=VALUE]`, `--build-arg-file PATH` | Repeatable arguments/files; explicit arguments override files. Bare names import host values when set. |
| `--build-context NAME=VALUE`, `--ignorefile PATH` | Supply named inputs or override automatic ignore-file selection. |
| `--tag`, `--push` | Repeatable local names/transfer destinations; `--push` publishes the tagged registry result. |
| `--platform` | Repeated/comma-separated Linux platforms; defaults to host platform. |
| `--jobs` | Total stage/platform concurrency; default 1, zero unlimited. |
| `--pull` | Image inputs: `missing` (default), `always`, `newer`, `never`. Bare `--pull` means always. |
| `--source-policy-file PATH` | Apply a BuildKit-format image source policy using Buildah's ALLOW, DENY, and CONVERT rules. |
| `--no-cache`, `--cache-ttl` | Bypass result reads or limit wall-clock publication age. Fresh results are still written. |
| `--cache-from`, `--cache-to` | Read or write caches; repeatable `oci-layout:PATH` or `registry:HOST/REPOSITORY`. Supply both to read and write the same cache. |
| `--network`, `--add-host` | Default RUN network and repeatable host mappings. Authored RUN network values override the default. `--network=host` also authorizes host-network requests. |
| `--secret`, `--ssh`, `--allow` | Supply credentials or authorize elevated RUN behavior; see [security](../guides/security.md). |
| `--mount SPEC` | Add a comma-separated RUN mount specification to every RUN, after its authored mounts. Repeatable; stage sources create graph dependencies. |
| `--source-date-epoch`, `--timestamp`, `--rewrite-timestamp` | Set creation time, force new-layer file times, or clamp newer times. Timestamp conflicts with the other two. |
| `--metadata-file` | Write result/platform/destination metadata. |
| `--quiet`, `--logfile`, `--logsplit` | Suppress progress or record it, optionally per platform. |

Image builds also expose `--format oci|docker`, filesystem `--output`, metadata controls, `--all-platforms`, `--manifest`, signing, SBOM scanners, and confidential workload conversion. Some require native runtime capabilities or external scanner images.

`--iidfile PATH` writes an algorithm-prefixed image ID for one platform or an index digest for several. `--iidfile-raw PATH` (alias `--raw-iidfile`) writes the image ID without its prefix and requires one platform. Neither adds a trailing newline.

Image-layer compression controls are `--compression-format gzip|zstd|zstd:chunked`, `--compression-level N`, and `--force-compression`. Specifying a format defaults force-compression to true; set `--force-compression=false` to permit reuse of compatible compressed blobs. See [output compression](execution.md#output-compression) for format and storage behavior.

### Named inputs

`--build-context NAME=VALUE` accepts local directories, `docker-image://REFERENCE`, selected `oci-layout://PATH:TAG`, public HTTP(S) tar archives, and HTTP(S)/Git/SSH/SCP-style Git repositories.

Git query options are `branch`, `tag`, or `ref`; optional `subdir` and `checksum`/`commit`; `submodules=false`; `keep-git-dir=true`; `mtime=commit`; and `fetch-by-commit=true`. The last requires a full lowercase commit SHA in `checksum`. Nested submodules fetch by default. Legacy `#ref:subdir` remains accepted. These query controls belong to named contexts; ADD retains its instruction-level Git options.

## Destinations

| Destination | Images | Components |
| --- | --- | --- |
| Plain `NAME` or `local:NAME` | Local tag. | Local tag. |
| `registry:HOST/REPOSITORY:TAG` | Registry image/index. | Registry artifact/index. |
| `oci-archive:PATH` | OCI archive. | OCI archive. |
| `docker:NAME` | Docker Engine transfer. | Unsupported. |

Bare digests select immutable local artifacts. Invoke a local component tag with `local:NAME`; registry references do not search the local component store. See [storage](../guides/storage.md) for platform selection and digest preservation. A failed post-build transfer reports the retained result; retry with `coopr copy` or `coopr component copy`.

Image and component `save` write archive bytes to stdout unless `--output FILE` is supplied. `load` reads stdin unless `--input FILE` is supplied. Image archives use the native image loader and retain configuration and layer history. An OCI archive holds one image or multi-platform index; Docker archives can hold multiple requested images. Component archives retain the complete component index and its packages. Loading a named component archive restores its name; `component load --tag NAME` overrides it. Pulling a component without `--tag` prints its locally usable digest.

Image loading imports runnable platforms. Descriptors with missing or `unknown` platforms, such as BuildKit attestations, are omitted from the stored index; its digest changes when descriptors are removed. Runnable-only indexes retain their original manifest bytes.

The `exists` commands are silent and return 0 when present, 1 when absent, and 125 for storage errors. `manifest remove LIST DIGEST` removes one member; `manifest rm LIST...` deletes lists. `manifest push` defaults to `--all=true`; false publishes the index alone. `manifest create` and `add` use `--all` to include every member of an input index. Registry flags use the same authentication, TLS configuration, and retry settings as builds and copy.

## Configuration and lifecycle

Global storage/runtime settings live in [configuration](configuration.md). Image pruning follows Podman's image rules: the default retains named images, images used by containers, and intermediate layers needed by another image or manifest list. `image prune --all` also removes unused named images; `--build-cache` additionally clears persistent RUN cache mounts. `--all` also clears those mounts. `system prune` applies the same image rules and prunes unnamed component content; it does not manage Podman containers, pods, networks, or volumes.

Both prune commands support `--dry-run`, which previews initial image candidates without changing data. Recursive pruning can remove additional parent images. See [storage](../guides/storage.md#inspect-and-maintain) for examples and cache-mount scope.

`--layers=false` reruns filesystem instructions into one new layer; `--rm` and `--force-rm` default to true. `--save-stages` retains completed intermediate stage images. `--stage-labels` requires it and adds `io.buildah.stage.name` and `io.buildah.stage.base` labels to stages with instructions; a FROM-only stage keeps the base image unchanged. `--compat-volumes` discards RUN changes under declared image volumes while retaining COPY/ADD changes. Resource controls depend on host cgroup and namespace permissions.

Global `--log-level` controls diagnostic stderr logs: `trace`, `debug`, `info`, `warn` (or `warning`), `error`, `fatal`, `panic`. Default `warn` shows warnings/errors.

Build progress goes to stderr; the final reference/digest goes to stdout. Progress includes `STEP i/n`, checkpoint IDs, `--> Using cache`, numbered stages, platform/component prefixes, final `COMMIT`, and `Successfully tagged` for named image outputs. With `--output type=tar,dest=-`, stdout carries the archive and stderr carries the final result. `--quiet` suppresses progress; `--logfile` still records it.
