# Migrate Stoke to the public client

This is a consumer migration plan, not a change to Stoke's implementation.
The Coopr follow-up provides `github.com/strio-dev/coopr/client` and shared
`github.com/strio-dev/coopr/api` types. It keeps the direct execution library,
Buildah/Podman storage and CLI behavior. Stoke continues to own workflows,
matrices, prerequisite/success gates and the service process lifetime.

## Release requirements

Release work remains held. Do not add an untagged module dependency, local
replacement or guessed future version to Stoke. Coopr `0.5.0` predates this
boundary. After separate release authorization, select a published Coopr module
containing `client`/`api` and an executable serving API v1 with the needed
capabilities. They may have different executable/module version strings;
protocol and per-feature capability negotiation are the compatibility contract.

Keep native engine distribution separate from the Go dependency. Importing the
client requires neither Buildah/native storage packages nor CGO. Running the
external Coopr executable still needs the engine's normal host/runtime setup.

## Replace duplicated protocol ownership

| Current Stoke surface | Migration |
| --- | --- |
| `internal/cooprwire/types.go` | Replace duplicate wire structs with public `api` types; remove this file when references/tests are migrated. |
| `internal/engine/client.go` HTTP/NDJSON code | Delegate to `client.Client.Execute`; remove duplicated framing, bounds, structured-error and cancellation code. |
| Engine client native build/component/publication wrappers | Construct `api.Request` values and call the same client invocation method. |
| `internal/engine/model.go` | Keep only workflow/compiler adapters still needed; preserve literal operation semantics and cache-availability handling. |
| Runner capability preflight | Derive requirements from the selected workflow, then use `Negotiate`; retain useful task context in errors. |
| `internal/engine/service.go` | Retain executable discovery, private directory, parent pipe, startup/stop/drain behavior. Replace private transport access with `CloseIdleConnections`. |
| `internal/engine/cache.go` and CLI checks | Keep invocation syntax checks where still needed; backend parsing remains authoritative. Avoid adding a second reference parser. |

Use public `api.Request`, `Options`, `Result`, native `Build` and `Publish`
values directly at the transport boundary. The client does not recreate Stoke's
literal operation interfaces or choose workflow scheduling. Preserve partial
publication results, optional native cache statistics, metadata and dropped
progress counts during any remaining result projection.

## Preserve lifetime and failure gates

Create the client for the service instance owned by that Stoke invocation.
Readiness may call `Info` with a short startup context; subsequent `Negotiate`
and requests reuse its validated snapshot. `Info` refreshes deliberately.
Do not impose an executable-version equality check or require every capability
for a workflow that uses only a subset.

Keep Stoke's owned service alive when the workflow context cancels, until
request reconciliation has finished. Its parent pipe/process shutdown still
belongs to Stoke. Do not pass the workflow cancellation context as the service's
entire lifetime or let client idle-connection closure stand in for service drain.

Preserve cleanup-failure precedence: `errors.Is(err, client.ErrCleanupFailed)`
must not become a normal canceled job. Keep `ExecutionError.ID` and underlying
`*api.Error` for diagnostics/reconciliation. A malformed or disconnected stream
must not imply success. Confirmed registration refusals must not cancel an
already registered ID, and a failed execution acknowledgement is not by itself
an incomplete cleanup.

Stream callbacks remain synchronous and validate events even if the caller
discards observations. Shared writers/callbacks require synchronization. Keep
progress optional unless a workflow explicitly requires the capability.

## Consumer qualification before adoption

After release selection and the separate Stoke implementation change:

1. Build Stoke with CGO disabled and inspect its dependency graph; no Coopr root
   execution package or native backend may reappear.
2. Port protocol/error regressions to the public client boundary, retaining
   scheduler-specific failure classification and service-lifetime tests in Stoke.
3. Test supported API v1/capability subsets and clear missing-feature failures.
4. Run the final Stoke checks and native literal/image/component/publication,
   matrix/DAG, cancellation, reusable-input, registry-cache and source-unavailable
   Bale relocation fixtures with the selected executable.
5. Preserve cold/warm cache behavior and all workflow success gates. Reuse the
   completed cache benchmark evidence as historical context; claim a new speed
   improvement only after its existing frozen performance gate passes.

Do not add executor adapters, service fleets, BuildKit sessions/workers or a new
content store during this migration. No dashboard, CDEvents, OpenTelemetry or
attestation changes are included.
