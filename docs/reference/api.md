# Local execution API v1

This reference describes the unreleased local `coopr system service` implementation. Coopr `0.5.0` does not serve this API. The transport-independent schema is maintained in `api/types.go` in the source checkout; clients need not import Coopr's native root package or server implementation.

## Transport and endpoints

HTTP runs over a private Unix socket. There is no TCP listener or automatic connection to an external daemon.

| Method and path | Result |
| --- | --- |
| `GET /v1/info` | JSON `apiVersion`, binary `version` and `capabilities`. Required API version: integer `1`. |
| `POST /v1/executions/ID` | Registers one execution and streams `application/x-ndjson` log/result events. |
| `GET /v1/executions/ID` | JSON status for a known ID, or 404. |
| `DELETE /v1/executions/ID` | Cancel and wait for backend return, then JSON status. Unknown IDs become canceled tombstones. |

`ID` is exactly 32 lowercase hexadecimal characters. Reusing an ID returns 409, including after cancellation-before-start. The service retains at most 4,096 records/tombstones during its lifetime; further registrations return 429. Shutdown rejects new execution admission with 503. Transport/admission errors use ordinary HTTP errors; admitted validation/execution failures use the terminal stream event.

POST requests contain exactly one JSON object, with unknown fields rejected, and are limited to 4 MiB. Event consumers must enforce the 1 MiB per-event limit. An absent terminal event, invalid event type, malformed event or interrupted connection is an uncertain result: cancel the known ID and wait instead of inferring success.

The `registry-cache` capability advertises `options.cache.registryFrom` and
`registryTo`. Require this capability before sending those options; API version
`1` alone does not establish support in an older development executable.

## Client compatibility

The [public Go client](../guides/client.md) uses these same API types. API major
version and required capabilities determine compatibility; executable version
strings need not match. Additive optional response fields and capability names
are permitted within response limits. Requests remain strict: a new optional
request feature needs its capability before it is sent. `build-platform` enables
nonempty native `build.platform`. `named-contexts`
advertises `options.contexts` and named COPY sources. `run-mounts-v2` adds named
RUN contexts, image-or-artifact RUN file inputs, explicit live `hostPath` binds,
optional host `relabel`, and the persistent `options.cache.mountDir` parent.
Require it for these features; API v1 alone does not establish their availability. Known v1 progress may be
validated and ignored; future semantic event kinds require negotiation or a
major revision. Unsupported majors do not fall back to another API.

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

This is a request-shape example: provide your own existing input/context, suitable native policy, runtime and fresh outputs. It is not a ready-made host configuration.

| Field | Contract |
| --- | --- |
| `kind` | One of `execute`, `build`, `component`, `component-load`, `publish`. |
| `program` | Required only for `execute`. |
| `build` | Required only for `build`/`component`; these kinds use the same payload shape. |
| `publish` | Required only for `publish`. |
| `componentLoad` | Required only for `component-load`; archive and explicit component store. |
| `options` | Explicit resource, output, cache and registry settings. |

Mismatched payload fields and operation kinds are errors. Literal requests do not generate Coopr source or invoke KDL ARG/ENV expansion.

### Literal programs

`program` contains optional `platform`, topologically ordered `states`, and `output` selecting the final state. Each state has `id`, `from` and optional ordered `operations`. A source selects exactly one `image` or earlier `state`.

For COPY, a source may instead select `{"context":"assets"}` from the local
directories declared in `options.contexts`. Context selection is exclusive with
`image` and `state`. Named contexts cannot be state bases. RUN bind mounts may
select `from.context` when `run-mounts-v2` is available. An omitted COPY source
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
conservatively disable result-cache reuse for the typed invocation. A named
`from.context` uses snapshotted named-context materialization. An external
`from.image` can resolve to a verified OCI artifact raw-file view; artifact
mounts require `readOnly: true`. Cache sharing supports `shared` and `locked`;
the current Buildah backend does not implement `private`. Supported types and native semantics follow the [Go execution library](../guides/library.md). Unsupported/incompatible fields are rejected. The typed graph is not a workflow dependency scheduler: states unreachable from the output can be pruned; an external test gate belongs to the caller's workflow layer.

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

This object is the `build` payload inside the request envelope; it is not a complete request. `files` is a nonempty ordered list of existing absolute regular files. `platform`, `target`, `args` and `componentStoreDir` are optional. `contextDir` is required. Coopr owns parsing, expansion, component resolution and native planning. The API exposes this selected set of frontend options; it does not imply that every CLI flag is a wire field.

`platform` selects one normalized Linux default (`linux/arch[/variant]`) for
native definition planning. Explicit per-stage FROM platform overrides remain
in effect. A nonempty selection requires `build-platform`; omitting it retains
the native default. Literal programs continue to use their existing
`program.platform` field and capability contract.

For a single native image result, `result.platform` reports the actual final
image configuration, which can differ from the requested default when the final
FROM overrides it. Component results report their platform-qualified artifact
selection rather than claiming a runnable image. Ambiguous multi-platform
metadata leaves the singular result field empty. The full native selection list
remains in `metadata["coopr.platforms"]`.

Foreign filesystem assembly and cross-compilation do not inherently need
emulation. A RUN that executes foreign binaries needs the runner's existing
Buildah-compatible binfmt/QEMU setup. Coopr supplies no emulator installer,
registration or alternate executor. Genuine exec-format failures retain the
native cause and identify this runner prerequisite.

`build` returns a native image ID and exports an OCI image layout. `component` returns a component reference/digest and exports an OCI archive at `options.output.oci`; it rejects `options.output.filesystem`. An archive is a file, not an image-layout directory.

### Explicit publication

For `kind: component-load`, capability `component-load` is required. Send only
`componentLoad: {"archive": "/absolute/artifacts/component.tar", "componentStoreDir": "/absolute/state/components"}`
and empty `options`. Both paths must be absolute; the archive must be a regular
file outside the selected component store. No native image store, runtime,
registry access, or build output is needed. The archive stays caller-owned.

The operation reuses the native OCI component loader: it verifies the complete
graph and relocation bindings before committing it through the existing store
locking. It does not unpack arbitrary archive paths or load a runnable image.
`manifestDigest` always returns the verified root digest. Without `name`,
`reference` also returns that bare digest; lookup remains ambiguous if the store
contains different delivery contexts for the same root. The optional `name` field
requires capability `component-load-name` and accepts a native local component
name (including an optional tag, but no digest). It selects this archive's delivery
context and returns a canonical `name:tag@sha256:...` reference with a fixed
content digest. The name selects mutable local delivery metadata: callers must
use distinct names for distinct deliveries and must not reassign them while
those selections are in use. The qualified name does not cryptographically pin
delivery metadata. Use the returned reference in native arguments or publication with
the **same explicit component store**. Existing execution streaming, cancellation
and cleanup acknowledgement apply; a canceled import is not a successful output.

For `kind: publish`, the `publish` payload is:

```json
{
  "artifact": "image",
  "source": "CALLER_STORE_IMAGE_ID",
  "destinations": ["oci:/absolute/exports/delivery"],
  "componentStoreDir": "/absolute/state/components"
}
```

`artifact` is `image` or `component`. `source` and nonempty `destinations` are required. The source must be meaningful in the supplied native/component store. Registry destinations perform explicit external writes; local destinations still write caller-selected files. Publication uses `destinations`, so build `output.oci`/`filesystem` must be absent. Results retain per-destination `destination`, `reference`, `status` and optional `error`; publication across destinations is not atomic.

The `publish-oci-layout` capability additionally accepts an explicit original
image layout, independently of the native image store:

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
match the required SHA-256 digest. Index/layout/manifest documents are bounded
regular files; metadata links, ambiguous indices, wrong digests and component
sources fail validation before transfer. The source must not overlap execution
storage/cache or filesystem destinations. Components retain their ordinary
verified component-store publication path. `sourceDigest` is rejected for
ordinary native/component sources.

This route reuses the existing OCI-root transfer, registry authentication, TLS
and signature-policy handling. Registry publication preserves the selected
manifest digest without native import, re-export or producer replay. It does
not select a host platform or recover missing content from a cache. Keep the
caller-owned source graph stable until publication completes; ordinary local
store activity/lock bookkeeping can be created beside its content. The API
still reports each destination separately and supports cancellation; it adds
no remote-source lookup or cross-destination transaction.

For filesystem destinations (`oci`, `oci-archive`, `dir`, `docker-archive`), the final absolute path must be unused and its parent must already exist. API publication transfers into a private sibling staging directory, then promotes the completed artifact without replacing any existing file, directory or symlink. A concurrent creator keeps its destination intact and that publication fails. Cancellation or transfer failure before promotion removes only the owned staging directory. Returned references use the authored destination rather than the staging path. This policy applies to API publication; ordinary CLI transfer behavior is unchanged.

## Options and ownership

| Object | Fields |
| --- | --- |
| `store` | Absolute nonoverlapping `runRoot` and `graphRoot`; optional `driver`, `driverOptions`. |
| `output` | Required fresh absolute `oci` for execution/build/component; optional fresh absolute `filesystem` for image/literal builds. Parents must already exist. |
| `cache` | Optional absolute `localDir` and separate private `mountDir`, boolean `noCache`, and independent `registryFrom`/`registryTo` repository arrays. Coopr owns identities, invalidation and coordinated reuse. |
| `registry` | Optional absolute `authFile`, `certDir`, `signaturePolicyPath`; `pullPolicy`, `tlsVerify`. |
| `contexts` | Optional mapping of native lowercase context names to existing absolute local directories. `build` and `scratch` are reserved; unknown COPY context names are errors. |
| `network` | Optional native RUN network default for `execute`, `build` and `component`; requires `run-network`. |
| Other options | Optional absolute `contextDir`, `runtime`, `isolation`, nonnegative `jobs`. |

Keep stores, cache, component store and outputs outside context and away from one another as required by validation. Existing outputs are not overwritten. Native stores/cache and successful outputs remain caller-owned. Multiple output promotions and publication destinations are not one atomic transaction. A failed run can leave partial evidence that must not be labeled a successful artifact.

For literal programs and native image/component builds, `registryFrom` reads
portable results and `registryTo` writes them. They may be used independently,
together, or with `localDir`. Values are bare registry repository names without
tags or digests, using the same native normalization as CLI `--cache-from` and
`--cache-to`; they are not BuildKit cache-spec strings or image transports.
Invalid names fail request validation before execution resources are created.
Registry authentication, TLS and policy use the request's `registry` options.

For example, a cache object can read one repository while writing another:

```json
{
  "registryFrom": ["registry.example.com/team/cache"],
  "registryTo": ["registry.example.com/team/cache-candidate"]
}
```

The cache format and failure policy are Coopr's existing [portable cache
semantics](../guides/caching.md). `noCache` bypasses reads while fresh results may
still be written. Cache mounts are not transferred. Publication requests do not
read or write execution cache entries; their explicit destinations remain
separate from build cache repositories.

These separation checks include every named local context. Input directories
may overlap each other or the primary context intentionally. Native `build`
and `component` requests receive the same named directories through Coopr's
existing build-context interface, so definitions can select them with
`copy "helper.sh" "/helper.sh" from="assets"`. The caller's primary context is
unchanged. Named contexts supplied while packaging a Coopr component are not
automatically retained as invocation overrides; use Coopr PACKAGE capture when
the resulting component needs self-contained files.

## Events and status

The `operation-progress` capability advertises typed observation events. Their
timestamps describe measured work; native workers replay them after completion.
Human log events continue streaming during execution.

```json
{"type":"stdout","data":"aGVsbG8K"}
{"type":"stderr","data":"ZGlhZ25vc3RpYwo="}
{"type":"progress","progress":{"id":"PHASE_ID","instruction":"copy","operation":2,"phase":"lookup","state":"completed","cache":"hit","started":"2026-11-02T00:00:00Z","completed":"2026-11-02T00:00:00.001Z","durationNanos":1000000}}
{"type":"result","result":{"imageID":"EXAMPLE_ID","oci":"/absolute/exports/image"}}
```

`data` is base64-encoded bytes; decode it before writing to stdout/stderr. Chunk boundaries need not coincide with lines or UTF-8 characters. The service chunks log writes; clients also bound every decoded event. One terminal `type: result` event carries a `result` object and optional `error` object with `code` and `message`. A result may retain partial output information on failure. Clients must validate the terminal event and its error, not merely successful HTTP transport.

Result fields are optional according to operation: `imageID`, `manifestDigest`, `platform`, `oci`, `filesystem`, `reference`, `cache`, `metadata`, `destinations`. Literal execution supplies portable-cache counters (`Hits`, `Misses`, `Stored`, `Skipped`, `Errors`). Native frontend build/component results currently omit `cache`; absence means unavailable, not zero activity. Do not synthesize zero counters or claim full cache statistics from omitted metadata.

Progress states are `started`, `completed`, `failed` and `canceled`. Terminal
phase observations carry a completion time and nonnegative `durationNanos`;
failed/canceled phases can include `error`. `stage`, `instruction`, `operation`
and `cache` provide attribution. IDs correlate each start/completion pair.
Durations use a monotonic clock; serialized wall-clock timestamps need not be
ordered after a clock adjustment. Lookup can include restore/checkpoint work,
so intervals overlap.

Capture retains up to 1,024 phase IDs/2,048 events per worker/owning plan. The
optional terminal `progressDropped` field counts omitted phases reported by
workers, including handled native build/component failures. Zero or absence
does not establish trace completeness after an abrupt worker kill. Native CLI
metadata retains the bounded trace, while API terminal `metadata` omits
`coopr.operation.progress` and its duplicated drop count; observations already
arrive as separate events. Clients can ignore valid observations, but must
continue validating the stream and terminal result.

For exported artifacts, `manifestDigest` identifies the root descriptor in the actual OCI layout or archive. Native frontend `metadata` can contain a different pre-export manifest identity after transport conversion.

If a terminal result exceeds the event limit, the service omits its result payload and returns `result_too_large` for an otherwise successful execution. An existing failure code, especially `cleanup_failed`, is preserved with a bounded message. Successful output files can already exist when this reporting failure occurs. DELETE acknowledgements omit the result payload and retain terminal state/error so large metadata cannot prevent cleanup acknowledgement.

Status has `id`, `state`, optional `result`, optional `error`. States are `running`, `succeeded`, `failed`, `canceled`. Error codes include `invalid_request`, `execution_failed`, `canceled`, `cleanup_failed` and `internal`; admission failures can additionally report `capacity`, `duplicate` or `shutdown`. Preserve an acknowledged `cleanup_failed` error rather than translating it to a normal canceled result.

## Cancellation and service shutdown

An admitted POST is bound to its connection and service context. Loss of the stream cancels that backend, but the caller must still use DELETE to establish its terminal state. DELETE waits for backend return; a canceled unknown-ID tombstone prevents late-start races. GET provides observations, not a cancellation acknowledgement.

The supervisor owns the service process and parent-lifetime pipe. Pipe EOF or SIGTERM cancels active operations and drains backend cleanup. Persistent resources remain caller-owned. If cleanup cannot be confirmed, report failure; killing the process is not equivalent to a clean acknowledgment. The API has no cross-account authentication layer or privilege-broker contract. See [the local API guide](../guides/api.md) and [security](../guides/security.md).

`options.network` uses the native build network syntax (for example `default`,
`none`, `host` or `ns:/absolute/path`). Empty preserves the native default.
Authored explicit RUN network selections override this default. Selecting `host`
explicitly grants the existing native host-network entitlement; namespace and
host access retain the caller's existing authority and runtime restrictions.
This controls RUN defaults, not a sandbox or offline enforcement policy:
registry image, component and cache traffic is separate. Invalid native network
values fail before execution resource creation. Publication and component archive
loading reject this option.
