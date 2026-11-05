# Service/client follow-up decisions

These decisions concern the Coopr-owned API/client follow-up. The completed
cache-performance qualification remains historical evidence in Stoke's
`evidence/cache-performance`; this follow-up does not repeat or redefine it.

## Preserve complete cache graphs

Keep the embedded BuildKit generic solver/cache manager, Buildah execution,
selected-result native restoration and the verified single-read graph-payload
path. `internal/cache/graph_store.go` is unchanged from the completed performance
candidate (SHA256 `122c8dcbd1b9fd255e02f2147bb5af171ef3310cf7b5ae0ea37a1b25fcd2b723`).
Its alias/conflict, integrity, source and complete-layout guarantees remain
regression requirements.

### Metadata-first network retrieval: deferred

`instructionGraphStore.LookupGraph` returns a complete verified private OCI
layout. `preparePortable` reads it before importing solver metadata;
`retainImages` deduplicates bytes in an invocation-owned layout afterward.
Selected-result restoration defers native image materialization, not these OCI
network downloads (`internal/buildah/instruction_solver_portable.go` and
`instruction_solver_lazy.go`).

Publication snapshots retain all physical alternatives associated with graph
nodes. `snapshot` exports each result and `publishPortable` writes a complete
record/layout to every selected destination. `PutGraph` verifies those complete
graphs before publication. A metadata-only read that later has to fetch every
retained alternative for local-cache seeding or publication merely moves the
same downloads later. Stage-worker relays also require owned complete layouts;
they do not transfer a remote lazy-fetch capability.

The completed fixed comparison already measures the relevant costs: source-image
registry blob bytes fell from 410,745,942 to 205,381,605; component-image fell
from 498,580,235 to 294,727,604. Both read-only registry-source and separate
read/write-mirror lanes were measured, while the writable local cache remained
active. The largest source layer still appears in three independent graph reads.
No additional network saving is established by inspecting metadata sooner.

A future experiment must distinguish truly read-only selected-result retrieval
from retained-alternative export and seeding. It needs source-aware result
ownership and publication decisions, without accepting incomplete verified
destinations. That is a larger cache-store contract change than this reusable
client needs, so it is deferred. No new speed or byte reduction is claimed.
Any future claimed optimization keeps the frozen gate: at least 15% better warm
median time or 25% fewer actual registry response bytes, with no fixed workload
median regressing more than 10% under the same conditions.

Incremental content hashing remains deferred under mutable inputs. No shared
content store, worker/snapshot migration or built-image back-links are added.

The completion review retains these decisions. Nix store paths and immutable OCI
inputs can justify specific identities, but a path spelling or timestamp does
not make arbitrary COPY inputs immutable. Preserve Buildah's exact copy stream,
metadata and content checks until a measured immutable-input or reliable
change-tracking contract replaces them. No new network-laziness claim follows
from reducing native imports.

## Progress renderer: deferred

The pinned BuildKit v0.33.0 `util/progress/progressui.Display.UpdateFrom` consumes
BuildKit `client.SolveStatus` values and terminal-display state. Coopr already
provides byte logs and backend-independent `api.OperationProgress` observations.
Supervised worker observations replay through existing result files after a
worker completes (`stage_worker.go`, `progress_buffer.go`); phase timing can
overlap, and dropped observations are explicit.

Adapting those batches into BuildKit vertices would add presentation and
terminal dependencies to a headless transport without supplying genuinely live
worker updates. A separate live worker telemetry channel was not authorized
here. Keep standard stream writers and optional progress callbacks; the caller
owns UI choices. No renderer dependency or new progress protocol is added.

## Keep semantic adapters explicit

The new client shares public API types rather than reproducing a wire schema.
Store/output/cache projections with identical Go fields use direct type
conversions in `apibackend`; registry projection remains explicit because the
execution library also exposes credentials not present on this API.

Native image and component option types retain separate constructors. Their
output/store behavior differs, and introducing a third common options type or
changing the public execution library would add integration code rather than
remove it. The existing frontend parser, shell/platform/reference/authentication
helpers and Buildah/Podman storage integration remain unchanged.

Stoke migration is a separate consumer change after a compatible client/API
release is authorized. The Coopr package provides transport, negotiation,
bounded protocol handling and cancellation; Stoke retains workflow planning,
matrices, prerequisites, success gates and parent-pipe/process supervision.

## Nix serialization and reusable package components

The current Nix interface builds packages/closures with Nix, then composes full
native Coopr components outside its sandbox. It reuses pinned nix2container Nix
machinery for the initial image. The earlier isolated Go-library proof matched
configuration and raw layers and rebuilt a fixed prebuilt exporter offline;
gzip bytes differed. That does not prove fewer total serializations, a better
end-to-end build, hermetic source packaging or production cancellation handling.
Selective helper adoption remains optional and outside Coopr core.

Reusable components assembled from Nix packages could serve repeated tool
packages, but need a coherent Coopr-owned packaging interface with native
PACKAGE/EXTEND/configuration and delivery behavior. The existing native
component builder is available; a new Nix assembler is not required to compose
existing components or consume the final environment in Stoke. Defer a new
assembler until a concrete consumer justifies its API, and do not manufacture
private planner metadata or reclassify components as runnable images.
