# Use the Go service client

Import `github.com/strio-dev/coopr/client` and the shared types in
`github.com/strio-dev/coopr/api` to call a caller-owned Coopr service. The client
uses the standard library and public API types. It does not import Buildah,
native storage or Coopr's embedding library, and it builds with CGO disabled.
The service executable still needs its normal native runtime prerequisites.

The client and service are unreleased. Coopr `0.5.0` contains neither this client
nor the service API. Use the explicitly qualified development builds until a
compatible module and executable release is authorized. Release work remains
on hold; this guide does not select an unpublished version for a consumer.

## Connect to an owned service

Create and supervise the private service as described in the
[local API guide](api.md). The caller owns its process, parent-lifetime pipe,
input paths, stores, caches and outputs. The client neither starts a process
nor connects to an always-on daemon.

Given an existing absolute socket path and a prepared `api.Request`:

```go
package integration

import (
    "context"
    "os"

    "github.com/strio-dev/coopr/api"
    "github.com/strio-dev/coopr/client"
)

func invoke(ctx context.Context, socket string, request api.Request) (api.Result, error) {
    connection, err := client.New(socket, client.Options{})
    if err != nil {
        return api.Result{}, err
    }
    defer connection.CloseIdleConnections()
    return connection.Execute(ctx, request, client.StreamOptions{
        Stdout: os.Stdout,
        Stderr: os.Stderr,
    })
}
```

`Execute` accepts the same request envelope for typed execution, native image
builds, native components and explicit publication. It preserves Coopr's
component/PACKAGE behavior and named contexts. It performs no workflow
scheduling, definition generation, artifact relocation or automatic publication.
See the [API reference](../reference/api.md) for paths and operation fields.

The separate `examples/client` Go module contains a complete command:

```sh
cd examples/client
CGO_ENABLED=0 GOTOOLCHAIN=local go run . \
  -socket /absolute/private/api.sock -request /absolute/request.json
```

It reads one bounded request object, sends logs to stderr, writes the structured
result to stdout and preserves a failing exit status even when a partial result
exists. Its local module replacement checks this checkout; it is not a
published module pin or a Stoke dependency change.

## Negotiate capabilities

`Info(ctx)` reads and validates the service identity. `Negotiate(ctx, caps...)`
checks the accepted identity, fetching Info if none has been accepted yet.
Execution automatically requires the selected operation capability and only the
optional request features it uses:

| Request | Required capability |
| --- | --- |
| Typed program | `execute` |
| Nonempty `Options.Network` on execution/build/component | `run-network` |
| Native image definition | `build` |
| Native component definition | `component` |
| Nonempty native `build.platform` | `build-platform` |
| Artifact publication | `publish` |
| Nonempty registry cache source/destination lists | `registry-cache` |
| Named local contexts or named-context sources | `named-contexts` |
| RUN mount using `from.image`, `from.context` or `hostPath`; persistent `cache.mountDir` | `run-mounts-v2` |

A RUN `from.image` can be an image or artifact reference, determined by the
engine. The client conservatively requires `run-mounts-v2` for that union rather
than resolving registry content itself. Ordinary programs without these features
retain their existing capability requirements.

API version `1` and the needed capabilities determine compatibility. The
executable's version string is informational; compatible client/server releases
do not need identical version numbers. Unknown optional response fields and
capability names are accepted within the response limits. Unsupported API
majors and missing features fail clearly before execution is posted.

A client belongs to one service instance. Successful Info is cached, including
during caller-owned readiness polling. Repeated negotiation and execution reuse
that snapshot. `Info` explicitly refreshes it; an unsuccessful refresh clears
the accepted snapshot. Construct a new client after replacing or restarting the
service rather than relying on stale capabilities.

Known v1 progress events are validated even with no callback. Observation is
optional: ordinary requests do not require `operation-progress`. Call
`Negotiate(ctx, api.CapabilityOperationProgress)` when observation support is
itself a requirement. Future semantic event kinds require explicit negotiation
or an API major revision; unknown discriminators are errors.

## Results, errors and cancellation

Always inspect both returned values. Native frontend results omit unavailable
cache counters; the client leaves that pointer nil. Publication can return
successful destination entries together with a structured failure for another
destination. The client preserves those outcomes and `*api.Error` codes through
`errors.As`; it does not convert them into zero results or ordinary log text.

An `*client.ExecutionError` retains `ID`, `Cause` and `CleanupError`. Request
preflight failures occur before POST and need no execution identity. Structured
HTTP admission failures remain available as `*client.HTTPError` and, when
present, `*api.Error`. Confirmed duplicate/capacity/shutdown refusals do not send
DELETE, which could otherwise cancel a previously registered execution.

After uncertain admission, connection loss, malformed/truncated events or a
writer error, the client cancels its POST context and independently sends DELETE
for that ID. It waits for a terminal, matching acknowledgement. The default
independent timeout is thirty seconds; `Options.CancellationTimeout` selects
another positive duration. POST is never automatically retried.

`errors.Is(err, client.ErrCleanupFailed)` means cleanup failed or could not be
confirmed. Preserve that distinction over ordinary context cancellation. An
acknowledged failed execution alone is not failed cleanup. Use
`CancelWait(ctx, execution.ID)` to retry reconciliation when an earlier
acknowledgement failed; `Get(ctx, id)` observes status without canceling.
Closing a connection or killing a service is not a cleanup acknowledgement.

## Bound streams and ownership

Requests are limited to 4 MiB and each NDJSON event to 1 MiB. Info responses are
limited to 64 KiB; status responses allow the event limit plus 4 KiB of envelope
headroom. Every response must contain the expected JSON object, without trailing
values or ambiguous control fields. Mixed payloads, invalid known progress,
unknown events, missing terminal results and post-terminal events are errors.
Additive optional fields do not relax those checks.

Stream writers and progress callbacks run synchronously, preserving order and
backpressure without an unbounded queue. They must return; blocked caller code
can delay cancellation return. Synchronize consumers shared by concurrent
executions. `CloseIdleConnections` releases idle HTTP resources, without
canceling jobs or shutting down the service.

## Qualification and migration

The independent `client-consumer` Nix check builds without Coopr's native inputs
or build tags. Its package inspection allows only the standard library, the
consumer, `api` and `client`, rejects CgoFiles and records `CGO_ENABLED=0`.
This proves client package/build independence, rather than claiming the entire
Coopr module manifest or service runtime is native-free.

Compatibility fixtures and native Unix-socket tests qualify transport errors,
cancellation, execution/publication concurrency, cache integrity and nested
component relocation with the source registry unavailable. Final validation
records distinguish those fresh results from the historical cache benchmarks.
ARM execution and the inherited-SELinux/empty-mount-label configuration remain
unqualified unless separately recorded otherwise.

See the [Stoke migration plan](../development/stoke-client-migration.md) for the
consumer changes that remain separate from this Coopr implementation, and the
[follow-up decisions](../development/service-client-decisions.md) for deferred
cache retrieval and renderer work. The existing [Go execution library](library.md)
remains available with its original native bootstrap contract.

## Restore a component archive

`Client.LoadComponent(ctx, api.ComponentLoad{Archive: archivePath,
ComponentStoreDir: storePath}, streams)` requires capability `component-load`.
Both paths are absolute. It verifies and stores the existing component graph;
it does not execute a build or extract package files into the caller context.
The returned `ManifestDigest` is always the verified root digest. Set optional
`Name` to a local component name to retain this archive's delivery selection; this
requires capability `component-load-name` and returns a canonical
`name:tag@sha256:...` `Reference`. Names use native component naming rules and
cannot contain a digest. With an empty `Name`, `Reference` is the bare root digest,
whose lookup remains ambiguous when several deliveries share that root. Native
builds using the returned reference and component publication must select the
same component store.
The digest fixes content, while the local name selects delivery metadata. Own a
distinct name for each delivery and do not reassign it while consumers use it;
`name@digest` alone does not make mutable delivery metadata immutable. Stoke's
exact imports use separate names within an invocation-private store.
Archive import uses the existing execution stream and cancellation protocol.

For execution and native build/component requests, `api.Options.Network` sets the
existing native RUN network default. Nonempty values require `run-network`;
empty preserves the native default. For example, `Network: "none"` selects no
network for RUN operations without an authored explicit network override. It
does not block registry image/component/cache traffic or provide an offline
sandbox. `host` explicitly grants the native host-network entitlement, and
`ns:/absolute/path` retains native namespace permissions and runtime checks.
Publication and archive loading reject nonempty `Network`.
