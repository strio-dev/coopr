# Local execution API v1

`coopr system service` serves HTTP over a private Unix socket. Request and response types are defined in `api/types.go`, which has no native engine dependencies. For a ready-to-use client, see the [Go client guide](../guides/client.md).

## Transport and endpoints

HTTP runs over a private Unix socket. There is no TCP listener or automatic connection to an external daemon.

| Method and path | Result |
| --- | --- |
| `GET /v1/info` | JSON `apiVersion`, binary `version` and `capabilities`. Required API version: integer `1`. |
| `POST /v1/executions/ID` | Registers one execution and streams `application/x-ndjson` log/result events. |
| `GET /v1/executions/ID` | JSON status for a known ID, or 404. |
| `DELETE /v1/executions/ID` | Cancel, wait for execution cleanup, and return JSON status. Unknown IDs are recorded as canceled. |

`ID` is exactly 32 lowercase hexadecimal characters. Reusing an ID returns 409, even if it was canceled before execution started. The service retains up to 4,096 IDs during its lifetime, including canceled IDs; registering another returns 429. New executions receive 503 during shutdown.

Requests rejected before execution starts return an HTTP error. Once a request is accepted, validation and execution failures are reported in the terminal stream event.

POST requests contain exactly one JSON object, with unknown fields rejected, and are limited to 4 MiB. Event consumers must enforce the 1 MiB per-event limit. An absent terminal event, invalid event type, malformed event or interrupted connection is an uncertain result: cancel the known ID and wait instead of inferring success.

The `registry-cache` capability advertises `options.cache.registryFrom` and
`registryTo`. Require this capability before sending those options; API version
`1` alone does not guarantee support.

## Client compatibility

The [public Go client](../guides/client.md) uses these same API types. Compatibility depends on the API major
version and required capabilities, not matching executable versions. Clients
may receive new optional response fields and capability names. Requests reject
unknown fields, so check the required capability before using an optional feature. `build-platform` enables
nonempty native `build.platform`. `named-contexts`
enables `options.contexts` and named COPY sources. `run-mounts` adds named
RUN contexts, image or artifact inputs for RUN mounts, live `hostPath` binds,
optional host `relabel`, and the persistent `options.cache.mountDir` directory.
`selected-file-exports` enables `options.output.exports` for image builds and
typed programs. The Go client requires this capability when exports are supplied.

Clients may validate and discard v1 progress events. New event kinds require
capability negotiation or a new API major version. Clients must reject
unsupported major versions rather than attempting a fallback.

## Request envelope

```json
{
  "kind": "execute",
  "program": {
    "states": [
      {
        "id": "result",
        "from": {"image": "scratch"},
        "operations": [
          {"kind": "copy", "sources": ["message.txt"], "destination": "/message.txt"}
        ]
      }
    ],
    "output": "result"
  },
  "options": {
    "store": {
      "runRoot": "/absolute/state/runroot",
      "graphRoot": "/absolute/state/graphroot",
      "driver": "vfs"
    },
    "contextDir": "/absolute/project",
    "output": {"oci": "/absolute/exports/image"},
    "cache": {"localDir": "/absolute/cache"}
  }
}
```

Replace the example paths with your input directory, store and unused output paths. For a runnable example with host setup, follow the [API guide](../guides/api.md).

| Field | Contract |
| --- | --- |
| `kind` | One of `execute`, `build`, `component`, `component-load`, `publish`. |
| `program` | Required only for `execute`. |
| `build` | Required only for `build`/`component`; these kinds use the same payload shape. |
| `publish` | Required only for `publish`. |
| `componentLoad` | Required only for `component-load`; archive and explicit component store. |
| `options` | Explicit resource, output, cache and registry settings. |

Supply only the payload for the selected `kind`. Literal programs use values as supplied; they do not expand Coopr ARG or ENV expressions.

### Literal programs

`program` contains optional `platform`, topologically ordered `states`, and `output` selecting the final state. Each state has `id`, `from` and optional ordered `operations`. A source selects exactly one `image` or earlier `state`.

For COPY, a source may instead select `{"context":"assets"}` from the local
directories declared in `options.contexts`. Context selection is exclusive with
`image` and `state`. Named contexts cannot be state bases. RUN bind mounts may
select `from.context` when `run-mounts` is available. An omitted COPY source
continues to use the primary `contextDir`.

| Operation `kind` | Fields |
| --- | --- |
| `run` | Literal `args` argv; optional `mounts`. Shell semantics require an explicit shell executable. |
| `copy` | Nonempty `sources`, `destination`, optional `from` source; omitted source uses `contextDir`. |
| `env` | Literal `name`, `value`. |
| `workdir` | `path`. |

Mount fields are `type`, `target`, optional `source`, `id`, `sharing`, `from`, `readOnly`, `hostPath` and `relabel`.
`hostPath` is an absolute live directory, exclusive with `from` and `source`; it
requires `bind`. Its optional `relabel` is `shared` or `private`, explicitly
requesting native SELinux labeling. The default does not relabel. Live mounts
disable result-cache reuse for the whole typed program. A named
`from.context` mounts a snapshot of that context. An external
`from.image` can resolve to a verified OCI artifact raw-file view; artifact
mounts require `readOnly: true`. Cache sharing supports `shared` and `locked`;
the current Buildah backend does not implement `private`. See the [Go execution library](../guides/library.md) for mount behavior. Unsupported fields and incompatible combinations are rejected.

States that do not contribute to the selected output may be skipped. Run required tests separately if their states are not dependencies of that output.

### Selected file exports

Image builds and typed programs can export literal files or directories from the
selected output state without exporting its complete root filesystem:

```json
"output": {
  "oci": "/absolute/exports/image",
  "exports": [
    {"source": "/out/tool", "destination": "/absolute/exports/tool"},
    {"source": "/reports", "destination": "/absolute/exports/reports"}
  ]
}
```

Sources are absolute container paths, without globs or `..` components. The
source's type determines whether a file or directory is copied. Destinations
are fresh absolute paths on the service host, separate from inputs, stores,
caches and other outputs. Directory exports reject special files and symlinks
that do not resolve within the selected directory, including dangling links.
Rootless exports strip setuid/setgid bits
and xattrs, as full filesystem exports do. RUN-only secret, cache and live
mount views are absent when files are captured.

Exports run after success, including a terminal cache hit, or after an ordinary
failed RUN in the selected output state before its builder is removed. The
execution remains failed. A failed prerequisite has no selected output
filesystem and yields skipped receipts. Cancellation or unconfirmed worker
cleanup prevents publication. Each result entry in `exports` reports `source`,
`destination`, `status` (`passed`, `failed` or `skipped`) and optional `error`;
inspect it even when execution returns an error. Each path is promoted after
confirmed cleanup without replacing an existing destination. Several exports
are not a single filesystem transaction.

Export paths do not change image/cache identity. A successful cached export
reads the selected terminal image; it does not restore ancestor filesystems.
The existing `output.filesystem` remains a separate, opt-in full-root export.

### Native definitions

For `kind: build` or `component`, supply:

```json
{
  "files": ["/absolute/project/image.coopr"],
  "platform": "linux/arm64",
  "target": "release",
  "args": {"VERSION": "example"},
  "componentStoreDir": "/absolute/state/components"
}
```

Place this object in the request's `build` field. `files` is a nonempty ordered list of existing absolute regular files. `platform`, `target`, `args` and `componentStoreDir` are optional. `contextDir` is required. Coopr parses the definitions, expands expressions, resolves components and plans the build. Only the fields listed here are supported; do not add CLI flags as extra `build` fields.

`platform` sets the default Linux platform (`linux/arch[/variant]`) for the
build. A stage can override it with its FROM platform. A nonempty value requires
`build-platform`; omit it to use the native default. For literal programs, set
`program.platform` instead.

For a single native image result, `result.platform` reports the actual final
image configuration, which can differ from the requested default when the final
FROM overrides it. Component results report the selected artifact
platform. When metadata identifies several platforms without a single
selection, `result.platform` is empty. Read `metadata["coopr.platforms"]` for
the full platform list.

Assembling a filesystem for another architecture or cross-compiling does not
by itself require emulation. A RUN that executes foreign binaries requires
Buildah-compatible binfmt/QEMU support on the service host. Configure it before
building; Coopr does not install or register emulators. Exec-format errors
include the native failure and identify the missing host support.

`build` returns a native image ID and exports an OCI image layout. `component` returns a component reference/digest and exports an OCI archive at `options.output.oci`; it rejects `options.output.filesystem`. An archive is a file, not an image-layout directory.

### Load components

For `kind: component-load`, capability `component-load` is required. Send only
`componentLoad: {"archive": "/absolute/artifacts/component.tar", "componentStoreDir": "/absolute/state/components"}`
and empty `options`. Both paths must be absolute; the archive must be a regular
file outside the selected component store. No native image store, runtime,
registry access, or build output is needed. The archive stays caller-owned.

The loader verifies the component graph and returns its root `manifestDigest`. Without `name`, `reference` is the bare digest, which is ambiguous when the store contains several delivery contexts for that root. Optional `name` requires `component-load-name` and selects a local delivery, returning `name:tag@sha256:...`. Use distinct names for distinct deliveries and keep them unchanged while in use. Use the returned reference with the same component store.

### Explicit publication

For `kind: publish`, the `publish` payload is:

```json
{
  "artifact": "image",
  "source": "CALLER_STORE_IMAGE_ID",
  "destinations": ["oci:/absolute/exports/delivery"],
  "componentStoreDir": "/absolute/state/components"
}
```

`artifact` is `image` or `component`. `source` and nonempty `destinations` are required. The source must exist in the supplied image or component store. Registry destinations upload the artifact; local destinations write it to disk. Publication uses `destinations`, so build `output.oci`/`filesystem` must be absent. Results retain per-destination `destination`, `reference`, `status` and optional `error`; publication across destinations is not atomic.

With `publish-oci-layout`, you can publish an exported OCI image layout
without importing it into the native image store:

```json
{
  "artifact": "image",
  "source": "oci:/absolute/verified-original-image",
  "sourceDigest": "sha256:EXACT_MANIFEST_DIGEST",
  "destinations": ["registry.example.com/team/image:tested"]
}
```

Require this capability before sending `sourceDigest` or an `oci:` source. The
source directory must exist, select exactly one runnable image manifest, and
match the required SHA-256 digest. Layout, index and manifest documents must be
regular files no larger than 1 MiB each. Symlinked metadata, ambiguous indices,
wrong digests and component sources are rejected before transfer. The source must not overlap execution
storage/cache or filesystem destinations. Components retain their ordinary
verified component-store publication path. `sourceDigest` is rejected for
ordinary native/component sources.

Publication preserves the selected manifest digest and uses the request's registry authentication, TLS and signature policy. Keep the source graph stable until publication completes. Missing content is an error; it is not recovered from a cache.

For filesystem destinations (`oci`, `oci-archive`, `dir`, `docker-archive`), the final absolute path must be unused and its parent must already exist. API publication transfers into a private sibling staging directory, then promotes the completed artifact without replacing any existing file, directory or symlink. A concurrent creator keeps its destination intact and that publication fails. Cancellation or transfer failure before promotion removes only the owned staging directory. Returned references use the requested destination rather than the staging path. This policy applies to API publication; ordinary CLI transfer behavior is unchanged.

## Options and ownership

| Object | Fields |
| --- | --- |
| `store` | Required absolute nonoverlapping `runRoot` and `graphRoot` for `execute`, `build`, `component` and `publish`, including OCI-layout publication. Omit for `component-load`. Optional `driver`, `driverOptions`. |
| `output` | Required fresh absolute `oci` for execution/build/component; optional fresh absolute `filesystem` for image/literal builds. Parents must already exist. |
| `cache` | Optional absolute `localDir` and separate private `mountDir`, boolean `noCache`, and independent `registryFrom`/`registryTo` repository arrays. Coopr determines which cached results can be reused. |
| `registry` | Optional absolute `authFile`, `certDir`, `signaturePolicyPath`; `pullPolicy`, `tlsVerify`. |
| `contexts` | Optional mapping of native lowercase context names to existing absolute local directories. `build` and `scratch` are reserved; unknown COPY context names are errors. |
| `network` | Optional native RUN network default for `execute`, `build` and `component`; requires `run-network`. |
| Other options | Optional absolute `contextDir`; optional `runtime` and `isolation`; nonnegative `jobs`. |

Keep stores, cache, component store and outputs outside context and away from one another as required by validation. Existing outputs are not overwritten. Native stores/cache and successful outputs remain caller-owned. Multiple output promotions and publication destinations are not one atomic transaction. A failed request can leave successfully exported outputs; inspect both the result and error before using them.

`cache.registryFrom` reads portable results and `registryTo` writes them. They accept bare repository names without tags or digests and can be used independently or alongside `localDir`. Registry authentication uses `registry`. `noCache` forces fresh execution while allowing cache writes. Cache mounts are not transferred; publication does not read or write execution cache entries. See [portable caching](../guides/caching.md).

Named contexts may overlap other inputs, but not stores, caches or outputs. Native definitions select them with `copy "helper.sh" "/helper.sh" from="assets"`. They are not retained as invocation overrides when packaging a component; use PACKAGE to capture self-contained files.

`network` accepts native values such as `default`, `none`, `host` or `ns:/absolute/path`. Empty preserves the native default; authored RUN settings override it. Explicit `host` grants the native host-network entitlement. This setting controls RUN networking, not registry traffic or offline enforcement. Publication and component loading reject it.

## Events and status

The `operation-progress` capability advertises typed progress events alongside streaming log output and the terminal result.

```json
{"type":"stdout","data":"aGVsbG8K"}
{"type":"stderr","data":"ZGlhZ25vc3RpYwo="}
{"type":"progress","progress":{"id":"PHASE_ID","instruction":"copy","operation":2,"phase":"lookup","state":"completed","cache":"hit","started":"2026-11-02T00:00:00Z","completed":"2026-11-02T00:00:00.001Z","durationNanos":1000000}}
{"type":"result","result":{"imageID":"EXAMPLE_ID","oci":"/absolute/exports/image"}}
```

`data` is base64-encoded bytes; decode it before writing to stdout/stderr. Chunk boundaries need not coincide with lines or UTF-8 characters. The service splits log writes into chunks; clients must enforce the event size limit. One terminal `type: result` event carries a `result` object and optional `error` object with `code` and `message`. A result may retain partial output information on failure. Clients must validate the terminal event and its error, not merely successful HTTP transport.

Result fields are optional according to operation: `imageID`, `manifestDigest`, `platform`, `oci`, `filesystem`, `reference`, `cache`, `metadata`, `destinations`. Literal execution supplies portable-cache counters (`Hits`, `Misses`, `Stored`, `Skipped`, `Errors`). Native frontend build/component results currently omit `cache`; absence means unavailable, not zero activity.

Progress states are `started`, `completed`, `failed` and `canceled`. Terminal
phase observations carry a completion time and nonnegative `durationNanos`;
failed/canceled phases can include `error`. `stage`, `instruction`, `operation`
and `cache` identify the work being reported. Match start and completion events by ID.
Durations use a monotonic clock; serialized wall-clock timestamps need not be
ordered after a clock adjustment. Lookup can include restore/checkpoint work,
so intervals overlap.

Progress reporting retains up to 1,024 phase IDs (2,048 events) per worker or build plan. The
optional terminal `progressDropped` field counts omitted phases reported by
workers, including handled native build/component failures. A zero or missing count
does not guarantee a complete trace if a worker was killed abruptly. Native CLI
metadata retains the bounded trace, while API terminal `metadata` omits
`coopr.operation.progress` and its duplicated drop count; observations already
arrive as separate events. Clients can ignore valid observations, but must
continue validating the stream and terminal result.

If worker cleanup cannot be confirmed, the operation reports `cleanup_failed` and does not promote its outputs. Preserve this error rather than treating it as an ordinary cancellation. See [cancellation and cleanup](../guides/security.md#cancellation-and-cleanup).

For exported artifacts, `manifestDigest` identifies the root descriptor in the actual OCI layout or archive. Native frontend `metadata` can contain a different pre-export manifest identity after transport conversion.

If a terminal result exceeds the event limit, the service omits its result payload and returns `result_too_large` for an otherwise successful execution. An existing failure code, especially `cleanup_failed`, is preserved with a bounded message. Successful output files can already exist when this reporting failure occurs. DELETE acknowledgements omit the result payload and retain terminal state/error so large metadata cannot prevent cleanup acknowledgement.

Status has `id`, `state`, optional `result`, optional `error`. States are `running`, `succeeded`, `failed`, `canceled`. Error codes include `invalid_request`, `execution_failed`, `canceled`, `cleanup_failed` and `internal`; admission failures can additionally report `capacity`, `duplicate` or `shutdown`. Preserve an acknowledged `cleanup_failed` error rather than translating it to a normal canceled result.

## Cancellation and service shutdown

An accepted POST is canceled if its connection is lost or the service shuts down. Losing the stream cancels the execution, but the caller must still use DELETE to confirm cleanup. DELETE waits for the backend to return. If the ID is unknown, the service records it as canceled so a delayed POST cannot start it. GET reads status without requesting or acknowledging cancellation.

The supervisor owns the service process and parent-lifetime pipe. Pipe EOF or SIGTERM cancels active operations and drains backend cleanup. Persistent resources remain caller-owned. If cleanup cannot be confirmed, report failure; killing the process is not equivalent to a clean acknowledgment. The API does not authenticate other accounts or restrict the service account's authority. See [the local API guide](../guides/api.md) and [security](../guides/security.md).
