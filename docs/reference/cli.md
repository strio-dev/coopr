# CLI

Use `coopr --help` or `coopr COMMAND --help` for available options in the installed binary. Command options appear under **Options**; inherited settings appear under **Global Options**. `coopr --version` prints its version. Image commands use the [native image store](configuration.md); components use their separate OCI store.

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
| `save IMAGE [NAME...]`, `image save` | Save an archive or image directory; use `--multi-image-archive` for several images. |
| `load`, `image load` | Import from stdin, a file/directory, or an HTTP(S) URL supplied with `--input`. |
| `exists IMAGE`, `image exists` | Test local image presence without pulling. |
| `history IMAGE`, `image history` | Show image history, with optional platform selection. |
| `component pull REF --tag NAME` | Pull all component platforms and optionally assign a local name. |
| `component push SOURCE DESTINATION` | Publish a stored component to a registry. |
| `component tag SOURCE NAME...` | Add local component names. |
| `component save REF`, `component load` | Export/import a component OCI archive. |
| `component exists REF` | Test local component presence without pulling. |
| `manifest create`, `add`, `annotate`, `inspect`, `push`, `remove`, `rm`, `exists` | Manage native image manifest lists. |
| `info` | Show effective host, runtime, version, and storage information; `--format` (`-f`) accepts JSON or a Go template. |
| `image ls [IMAGE]`, `images [IMAGE]` | List, filter, sort, or format local images. |
| `image inspect NAME...`, `inspect NAME...` | Inspect one or more local images. |
| `image rm NAME...`, `rmi NAME...` | Remove tags or images; `--all` selects all images. |
| `image prune` | Remove dangling images and unused instruction snapshots. |
| `component ls`, `components` | List locally named components. |
| `component inspect REF`, `component rm NAME...` | Inspect components or remove names. |
| `system df`, `system prune` | Report storage usage or prune images and unnamed component content. |

## Build controls

These controls apply to both build commands unless noted. Refer to the [execution reference](execution.md) for scope and cache rules.

| Option | Behavior |
| --- | --- |
| `--file`, `--from` | Read explicit definition files in the order supplied, or replace the first FROM image. Positional FILE uses its directory as context; `--file FILE [CONTEXT]` defaults to the current directory. Empty `--file` values are errors. |
| `--target` | Select a named output; default is the final stage. Component invocation cannot change the published target. |
| `--build-arg NAME[=VALUE]`, `--build-arg-file PATH` | Repeatable arguments/files; explicit arguments override files. Bare names import host values when set. Undeclared names produce a warning containing names only. |
| `--build-context NAME=VALUE`, `--ignorefile PATH` | Supply named inputs or override automatic ignore-file selection. |
| `--tag`, `--push` | Repeatable local names/transfer destinations; `--push` publishes the tagged registry result. |
| `--platform` | Repeated/comma-separated Linux platforms; defaults to host platform. |
| `--jobs` | Total stage/platform concurrency; default 1, zero unlimited. |
| `--pull` | Image inputs: `missing` (default), `always`, `newer`, `never`. Bare `--pull` means always. |
| `--source-policy-file PATH` | Apply a BuildKit-format image source policy using Buildah's ALLOW, DENY, and CONVERT rules. |
| `--no-cache`, `--cache-ttl` | Bypass result reads or limit wall-clock publication age. Fresh results are still written. |
| `--cache-from`, `--cache-to` | Read or write caches; repeatable repository names without tags/digests, or `type=local,src=PATH` for reads and `type=local,dest=PATH` for writes. Supply both to read and write the same cache. |
| `--network`, `--add-host` | Default RUN network and repeatable host mappings. Authored RUN network values override the default. `--network=host` also authorizes host-network requests. |
| `--secret`, `--ssh`, `--allow` | Supply credentials or authorize elevated RUN behavior; see [security](../guides/security.md). |
| `--mount SPEC` | Add a comma-separated RUN mount specification to every RUN, after its authored mounts. Repeatable; stage sources create graph dependencies. |
| `--source-date-epoch`, `--timestamp`, `--rewrite-timestamp` | Set creation time, force new-layer file times, or clamp newer times. Timestamp conflicts with the other two. |
| `--metadata-file` | Write result/platform/destination metadata. |
| `--quiet`, `--logfile`, `--logsplit` | Suppress progress, redirect output to a file, or use one logfile per platform. Quiet also suppresses logfile progress; final successful results are still recorded. |

Image builds also expose `--format oci|docker`, filesystem `--output`, metadata controls, `--all-platforms`, `--manifest`, signing, SBOM scanners, and confidential workload conversion. Some require native runtime capabilities or external scanner images.

`--iidfile PATH` writes the algorithm-prefixed native image ID, or native manifest-list ID for an aggregate output. `--iidfile-raw PATH` (alias `--raw-iidfile`) writes the image ID without its prefix and requires one platform. Neither adds a trailing newline. Either option suppresses the build's final ID line.

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

`component rm NAME...` removes local names and accepts the `local:NAME` prefix; it does not remove artifacts by digest.

Bare digests select immutable local artifacts. Invoke a local component tag with `local:NAME`; registry references do not search the local component store. See [storage](../guides/storage.md) for platform selection and digest preservation. A failed post-build transfer reports the retained result; retry with `coopr copy` or `coopr component copy`.

## Archives and directories

Image and component `save` write archive bytes to stdout unless `--output FILE` is supplied. Image `save --format` accepts `docker-archive` (default), `oci-archive`, `docker-dir`, or `oci-dir`; directory formats require `--output DIRECTORY`. For a Docker archive, operands after the first image are additional tags unless `--multi-image-archive` (`-m`) selects multiple images. OCI archives hold one image or complete stored index. `--compress` applies to Docker directory output; `--uncompressed` controls OCI output. Native image export may change compression and manifest/index digests while retaining filesystem layers and their order.

Image `load --input` accepts a local archive, image directory, or HTTP(S) archive URL; omission reads stdin. Loading retains configuration and layers, including distinct members that share a platform in an OCI index. Component archives retain the complete component index and its packages. Loading a named component archive restores its name; `component load --tag NAME` overrides it. Pulling a component without `--tag` prints its locally usable digest.

Image loading imports runnable platforms. Descriptors with missing or `unknown` platforms, such as BuildKit attestations, are omitted from the stored index; its digest changes when descriptors are removed. Runnable-only indexes retain their original metadata in native storage. Export updates member descriptors if their layer representation changes.

## Manifest lists

`manifest remove LIST DIGEST` removes one member; `manifest rm LIST...` deletes lists. `manifest push` defaults to `--all=true`; false publishes the index alone. `manifest create` and `add` use `--all` to include every member of an input index.

Create accepts repeatable `--annotation KEY=VALUE` for index annotations. Add and annotate accept instance annotations and `--os`, `--arch`, `--variant`, `--os-version`, `--os-features`, and `--features`. Annotate selects a member by image name or digest; `--index` applies annotations to the index.

`manifest inspect` checks the local list before fetching a registry manifest, with registry authentication and TLS options. `manifest rm --ignore` tolerates missing lists; `manifest push --rm` removes the local list only after a successful push. Registry options use the same authentication, TLS configuration, and retry settings as builds and copy.

## Image reports

`images [IMAGE]` accepts a name selector. Options include `--quiet`, `--digests`, `--no-trunc`, `--noheading`, `--all`, `--sort created|id|repository|size|tag`, and repeatable native `--filter KEY=VALUE`. `--all` includes intermediate images; Coopr's private cache names remain hidden. `--format json` returns structured data; Go templates select fields.

Image inspection accepts multiple names and `--format` (`-f`) with JSON or a Go template. Successful inspections are printed even if another operand fails. Default output is a JSON array, including for one image. Inspecting an index selects the host platform and returns that image's metadata; use `manifest inspect` for the raw index and its members. A foreign single-platform image can still be inspected. History uses readable ages and sizes by default. `--human=false` prints absolute dates; `--format json` retains the underlying history data, and Go templates can select individual fields.

## Transfer output and errors

Image pull writes image IDs to stdout. `pull --all-tags REPOSITORY` pulls every repository tag. When given several images, pull attempts each and reports failures after successful results. Pull, image push, and manifest push report transfer progress on stderr; `--quiet` suppresses that progress. Push commands write no success line to stdout; use `--digestfile PATH` to save the pushed digest.

Image push accepts `--format oci|v2s2|v2s1`; manifest push accepts `oci|v2s2`. Both support compression format/level/force options and `--remove-signatures`; manifest push also accepts `--add-compression` for additional compressed variants. Image push supports repeatable `--encryption-key` and `--encrypt-layer`, whose negative indexes count from the last layer. Specifying compression format defaults force-compression to true; explicit `--force-compression=false` permits compatible blob reuse.

Removal reports distinguish `Untagged:` names from `Deleted:` image IDs. Command errors go to stderr with an `Error:` prefix and normally return 125. The `exists` commands are silent: 0 means present, 1 means absent, and 125 means a storage error. Failed RUN commands preserve their exit status. Image removal returns 1 for missing-only failures, 2 for images in use, and 125 for other failures; successful removals are still reported. Option-parsing errors also print the command's help hint.

## Shell completion

Generate a completion script with `coopr completion bash`, `zsh`, `fish`, or `powershell`. `--file` (`-f`) writes it directly to a file; `--no-desc` omits descriptions. For the current Bash session:

```sh
source <(coopr completion bash)
```

Install persistent scripts in your shell's completion directory. Completion omits hidden options and avoids filename suggestions for registry usernames and passwords.

## Configuration and lifecycle

Global storage/runtime settings live in [configuration](configuration.md). Image pruning follows Podman's image rules: the default retains named images, images used by containers, and intermediate layers needed by another image or manifest list. `image prune --all` also removes unused named images; `--build-cache` additionally clears persistent RUN cache mounts. `--all` also clears those mounts. `system prune` applies the same image rules and prunes unnamed component content; it does not manage Podman containers, pods, networks, or volumes.

Both prune commands ask for confirmation before removing data; `--force` (`-f`) skips the prompt. `--dry-run` previews initial image candidates without changing data or asking for confirmation.

Repeatable `--filter label=KEY[=VALUE]`, `label!=KEY[=VALUE]`, and `until=TIME` restrict image candidates; filters do not select component blobs or RUN mount directories.

`system df --verbose` shows per-image usage, and `--format json` or a Go template formats the summary; format and verbose cannot be combined. Recursive pruning can remove additional parent images. See [storage](../guides/storage.md#inspect-and-maintain) for examples and cache-mount scope.

`--layers=false` reruns filesystem instructions into one new layer; `--rm` and `--force-rm` default to true. `--save-stages` retains completed intermediate stage images. `--stage-labels` requires it and adds `io.buildah.stage.name` and `io.buildah.stage.base` labels to stages with instructions; a FROM-only stage keeps the base image unchanged. `--compat-volumes` discards RUN changes under declared image volumes while retaining COPY/ADD changes. Resource controls depend on host cgroup and namespace permissions.

Global `--log-level` controls diagnostic stderr logs: `trace`, `debug`, `info`, `warn` (or `warning`), `error`, `fatal`, `panic`. Default `warn` shows warnings/errors.

Build progress goes to stderr; a successful image build prints its native image or manifest-list ID without an algorithm prefix to stdout. Progress includes `STEP i/n`, checkpoint IDs, `--> Using cache`, numbered stages, platform/component prefixes, final `COMMIT`, and `Successfully tagged` for named image outputs. With `--output type=tar,dest=-`, stdout carries the archive and stderr carries the final ID. `--quiet` suppresses progress, including progress written to a logfile. `--logfile PATH` redirects progress and the final result away from the terminal; `--logsplit` uses `PATH_OS_ARCH[_VARIANT]` for each platform. IID-file options also suppress the final ID in these logs. Component builds print their local reference or digest, with the same quiet/logfile routing.
