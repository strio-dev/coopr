# CLI

Run `coopr --help` or `coopr COMMAND --help` for the complete flags supported by your built binary. The following commands share the selected image store, while components retain their separate OCI store.

Global `--log-level` controls diagnostic logs on stderr and defaults to `warn`, showing warnings and errors. Use `--log-level=info` or `--log-level=debug` for additional diagnostics. Supported levels are `trace`, `debug`, `info`, `warn` (also `warning`), `error`, `fatal`, and `panic`. Build progress and RUN output use their existing output controls.

| Command | Purpose |
| --- | --- |
| `build [file\|context]` | Build an image from an explicit file; context operands require `--file FILE`. |
| `component build [file\|context]` | Build a component from an explicit file; context operands require `--file FILE`. |
| `copy SOURCE DESTINATION` | Copy a stored image or explicit engine source. |
| `component copy SOURCE DESTINATION` | Retag/export a component. |
| `image ls`, `images` | List locally named images. |
| `image inspect NAME`, `image rm NAME...` | Inspect or remove local image names. |
| `component ls`, `components` | List locally named components. |
| `component inspect REF`, `component rm NAME...` | Inspect components or remove names. |
| `system df`, `system prune` | Inspect/prune aggregate local state. |
| `cache df`, `cache prune` | Inspect/prune instruction-cache aliases and images. |

## Build controls

Both build commands accept definition/context selection, targets, arguments, platforms, network/runtime controls, credentials, secrets/SSH sources, caches, timestamps, and logs. Important defaults:

| Option | Behavior |
| --- | --- |
| `--file`, `--context`, `--from` | Choose definition/context or replace first base. |
| `--target` | Choose named output; final stage is default. |
| `--build-arg`, `--build-arg-file` | Supply arguments; explicit arguments override files. |
| `--tag` | Repeatable local name or explicit transfer destination. |
| `--platform` | Repeated/comma-separated Linux platforms; defaults to host platform. |
| `--jobs` | Shared stage/platform concurrency; default 1, zero unlimited. |
| `--pull-policy` | Image inputs: missing (default), always, newer, never. |
| `--no-cache`, `--cache-ttl` | Bypass result reads or limit publication age. |
| `--cache`, `--cache-from`, `--cache-to` | Read/write, read-only, or write-only portable caches. |
| `--network` | Default RUN mode; authored per-RUN values override it. |
| `--secret`, `--ssh`, `--allow` | Supply credentials or authorize elevated RUN behavior. |
| `--source-date-epoch`, `--timestamp` | Reproducible creation or forced layer file times. |
| `--quiet`, `--logfile`, `--logsplit` | Control progress/output files. |

Image builds additionally support `--format oci|docker`, filesystem `--output`, `--iidfile`, metadata controls, manifest append/discovery, signing, SBOM scanners, and confidential workload conversion. These can require native runtime capabilities and external scanner images; consult [security](../guides/security.md).

## Destinations

Plain build tags name local results. `local:NAME` aliases a stored image; `registry:HOST/REPOSITORY:TAG`, `oci-archive:PATH`, `podman:NAME`, and `docker:NAME` select explicit image copies. Components support registry/archive/local destinations, rather than engine stores. `--push --tag NAME` publishes to a registry.

Bare digests can select immutable local artifacts. A local component tag is invoked with `local:NAME`. Registry references do not silently search the component store. See [storage](../guides/storage.md) for platform and digest-preservation details.

## Configuration and lifecycle

Global `--image-store`, native storage flags, and runtime settings are described in [configuration](configuration.md). Prune supports `--dry-run`; named/in-use roots are protected. `--layers=false` reruns filesystem instructions into one new layer; `--rm` and `--force-rm` default to true. Resource controls remain subject to host cgroup and namespace permissions.

Build progress uses `STEP i/n` headers, followed by actual checkpoint image IDs
and `--> Using cache` when an instruction reuses a cached image. Multi-stage
builds number their stages; platform and component prefixes identify concurrent
or nested work. The final stage prints `COMMIT`, and completed named image
outputs print `Successfully tagged`. Progress goes to stderr; the final image
reference or digest remains on stdout. With `--output type=tar,dest=-`, stdout
contains the archive and the final result goes to stderr. `--quiet` suppresses progress, while
`--logfile` records it, including when `--quiet` is also selected.
