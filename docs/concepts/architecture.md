# Architecture

Coopr reads KDL v2, plans the selected output’s dependencies, and executes them through embedded Buildah. Images use a native graph; components use a separate OCI store.

| Part | Responsibility |
| --- | --- |
| Definition parser | Read instructions and options in authored order. |
| Planner | Resolve dependencies, argument scopes, outputs, and component phases. |
| Resolver and stores | Select Linux platform descriptors, verify bytes, and retain inputs. |
| Embedded Buildah | Apply filesystem and image-configuration changes. |
| Transfer | Copy results to local tags, archives, registries, or engine stores. |

The pipeline uses Go APIs, without generated Containerfiles, builder CLIs, or a builder daemon. RUN starts an OCI runtime; storage, networking, Git, SSH, and credentials may need host helpers. Explicit Docker destinations use the Engine API.

## State and dependencies

A stage holds a filesystem, image configuration, target platform, and argument scope. Its instructions execute in order. FROM, COPY/ADD sources, and RUN mount sources create dependencies on other stages or inputs.

The planner rejects cycles and ambiguous references. Independent stages can execute concurrently within the shared `--jobs` budget. Component packaging and invocation have different inputs; see [stages and component phases](stages.md).

## Portable artifacts

An image contains its filesystem layers and runtime configuration. A component contains a retained transformation, selected output, fixed arguments, target platform, and immutable package snapshots. Those snapshots let it run without the publisher's checkout.

Descriptor sizes and digests are verified. An outer component pin fixes its bytes, but mutable references inside it can change. See [security](../guides/security.md) for trust and permissions.

Coopr preserves image configuration fields it does not change. Cache identity omits layer history and rootfs provenance, which the final artifact retains. The [execution reference](../reference/execution.md) defines the detailed cache, metadata, and ONBUILD rules.
