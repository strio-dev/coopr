# Architecture

Coopr separates definition parsing, graph planning, OCI resolution, and execution. The definition and component artifact formats belong to Coopr; Buildah provides image-building primitives.

| Boundary | Responsibility |
| --- | --- |
| Definition | Parse ordered KDL v2 instructions and options. |
| Planner | Resolve stage dependencies, argument scopes, selected outputs, and component phase boundaries. |
| Resolver and stores | Select Linux platform descriptors, verify bytes, and retain image or component inputs. |
| Embedded Buildah | Apply filesystem and image-configuration operations to the planned state. |
| Transfer | Copy retained artifacts to explicit local, archive, registry, or engine destinations. |

The production pipeline uses Go APIs. It does not generate Containerfiles or invoke builder/engine CLIs. RUN starts the configured OCI runtime; networking, storage, Git, SSH, and credentials may require their configured host helpers. No builder daemon is required, though an explicit Docker destination requires Docker's Engine API.

## State and dependencies

A stage holds a Linux filesystem, logical image configuration, target platform, and argument scope. Instructions execute in order within that stage. FROM, COPY/ADD sources, and RUN mount sources introduce graph dependencies. The planner rejects cycles and ambiguous references before execution.

Independent stages can execute concurrently within the build's shared `--jobs` budget. Publication and invocation have different available inputs; see [component phases](stages.md).

## Portable artifacts

Images live in the selected native image graph. Components live in an OCI layout containing the retained invocation graph, selected output, fixed arguments, platform, and immutable package snapshots. This makes component invocation independent of the publisher's checkout.

Descriptor digests and sizes are checked before use. Digest pins select exact bytes; trust policy and runtime permissions remain separate concerns. An outer component pin does not freeze mutable image/component references nested inside it.

Coopr preserves raw image configuration it does not change. Portable state identity excludes layer history and rootfs provenance; the final artifact retains them. The [execution contract](../reference/execution.md) records cache identity, metadata, ONBUILD, and phase rules.
