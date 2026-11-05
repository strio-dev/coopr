# BuildKit cache adapters

Coopr imports the pinned BuildKit generic solver, cache manager, bbolt metadata storage and portable graph parser. Coopr supplies operation identities, immutable results and direct Buildah execution. There is no BuildKit daemon, LLB worker, replacement image store or containerd service.

The workflow graph belongs to clients such as Stoke. This adapter models Coopr's instruction dependencies inside one execution. Existing component and package aggregate caches remain separate scopes.

## Execution and identity

The existing stage loop retains frontend expansion, source measurement, authorization and component returns. Before an eligible operation executes, an asynchronous solver request either selects a complete result or supplies the immutable parent to execute. The stage loop performs the native operation and completes that request with a committed checkpoint. Cancellation drains the request before releasing execution resources.

Some mutable ADD sources require native application to measure their exact input digest. With no pending opaque checkpoint, a hit can replace that complete provisional builder with its verified cached result. When an opaque checkpoint is pending, such an operation remains ineligible so its changes cannot be absorbed into the wrong parent. HTTP/Git resolution, TLS validation and content measurement still happen before reuse.

An operation key includes execution configuration, measured sources, platform, runtime, backend module versions, graph-driver settings and relevant policy. Generated parent layer identities are represented by dependency edges. The parent layer count also bounds compatible history restoration; a different count causes a conservative miss. Source images and opaque boundaries retain concrete filesystem identity. Operations that cannot supply a sound reusable identity create boundaries; layer groups and dynamic component behavior keep their existing execution semantics. Forced dependencies propagate required execution through the solver.

An isolated stage worker returns its completed image and graph metadata to the parent. A later worker consumes that specific completed image through a concrete filesystem boundary, rather than allowing a new solver job to replace it with another physical alternative. Filesystem-producing components expose a consistent completed-image boundary whether they execute or hit an aggregate cache; metadata-only extensions preserve their caller endpoint. These boundaries preserve invocation results and can conservatively limit cross-boundary reuse.

A subsequent native history checkpoint can materialize an empty layer, including after a metadata-only component on scratch. If that changes the checkpoint's DiffIDs, the preserved endpoint is refreshed from the actual completed image while retaining forced ancestry. Parent verification still requires the mounted builder and selected result to have identical physical ancestry.

Results retain the exact image manifest, root-directory metadata and selected operation history boundary. A hit adopts the complete selected image. Current authored empty history, logical configuration and annotations are reconciled with its physical ancestry. History/layer inconsistencies fail restoration rather than producing a mixed image.

## Persistence and lifetime

Native images receive immutable per-result retention names. Result metadata is written atomically before its durable graph association becomes visible. Missing or pruned native images are unavailable cache results. Internal names remain cache indexes for maintenance, rather than user image tags. Source-name recovery excludes these retention names from public base-image annotations, including when another solver saves the source concurrently.

Eligible plan invocations expose verified portable alternatives through an invocation-owned key/result overlay. BuildKit selects a result before its `Load` callback imports that image into containers/storage. Unselected alternatives stay in verified OCI staging and can be re-exported without native unpacking. Pending references never enter the persistent native key database, where another process could otherwise prune references to images not yet materialized. A per-result process lock coordinates materialization and freshness updates across independent invocations.

Isolated stage workers retain eager native imports. Their existing relay transfers completed results, and making it transfer ownership of unmaterialized alternatives would require a larger lifecycle protocol. Keeping the existing import path there preserves all historical variants for the final publisher. This is an explicit integration-cost boundary, not a promise of selected-only imports on every execution path.

Each metadata operation takes a context-aware process lock, opens the upstream database, performs the operation and closes it. Walk methods collect rows before invoking callbacks, allowing BuildKit's cleanup callbacks to reenter storage. No database handle remains open during container execution, image transfer or caller callbacks. Independent CLI and API worker processes share this protocol.

## Portable graphs

The OCI graph artifact contains BuildKit's `CacheConfig`, result sidecars, executor metadata and complete selected image graphs. Every retained physical result for the selected graph is encoded. Coopr avoids `CacheChains.Marshal` as its sole serializer because that API selects one best result per record. Native manifests are exported once into a shared staging layout and reused across graph-prefix artifacts.

Native key links accompany the portable graph because output-normalized digests cannot be reversed into native operation digests. Import reconstructs and compares both representations, validates bounds and dependencies with the upstream parser, and verifies sidecars and complete image blobs before admitting invocation-local associations. Materialized results become durable before their result associations are persisted. Source nodes retain their keys and links without exporting a separate source-image result; declared source resolution remains authoritative. Execution checkpoints contain their complete selected ancestry. Imported cache images retain exact compressed layer representations before their staging layout is removed. Invalid reads warn and rebuild; explicit cache-write failures fail the build.

Read and write destinations are independent. A write-only destination is never queried. Export happens after successful execution, and imported results can seed explicitly writable destinations. A forced execution may import the existing graph to retain alternatives, while ignoring its execution results. RUN cache-mount contents are not part of the artifact.

This is an internal Coopr cache format. Reusing the upstream graph parser does not establish native BuildKit registry-cache compatibility. Concurrent registry publishers use ordinary registry tag replacement; consumers verify each complete published artifact.

Selected native imports reduce unpacking and native-store work. The portable reader still downloads and verifies the complete artifact. Metadata-first transfer was deferred: the common read-and-write flow republishes every retained alternative, so a useful total-byte reduction also needs source-aware export/seeding work. No registry-byte or elapsed-time speedup follows from a lower native import count.

## Observations

BuildKit's progress controller supplies paired phase lifecycles to a synchronous Coopr writer. The observer has no collector goroutine or unbounded progress queue; an absent observer avoids controller allocation. The public event type remains Coopr-owned.

Each supervised plan/stage result captures at most 1,024 phase IDs and 2,048 events. Completions for admitted phases remain retained; new phases beyond the limit increment a dropped-phase counter. Parents validate and replay the existing private JSON result payload after worker completion. Human-readable logs continue streaming during execution. Force-killed workers may have no timing response.

Phases describe preparation, lookup, actual selected-image restoration, execution and export. Lookup can enclose restoration and boundary checkpoint work; nested phases overlap and must not be summed as independent wall-clock totals. `durationNanos` uses the worker's monotonic elapsed time. Serialized wall-clock start/completion times can move backwards after a clock adjustment.

Native CLI metadata retains the bounded trace. The API streams observations and reports `progressDropped`, while omitting the duplicate trace from terminal metadata so telemetry cannot exhaust the terminal-event byte limit. Observations do not affect cache keys, authorization, output promotion or failure classification.

If observations would exceed the existing worker-result byte limit, response encoding omits that optional trace and adds its phase count to the dropped counter before retrying once. Required result data keeps the original byte limit and failure behavior.

Incremental COPY-input indexing was also deferred. Coopr's mutable caller contexts and Buildah-selected stream require exact change detection. BuildKit's full content hasher depends on immutable references/change tracking; filesystem filtering alone does not supply that contract. Current input probing and the probe/application recheck remain authoritative.
