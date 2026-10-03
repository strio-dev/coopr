# Stages and component phases

`from`, `package`, and `extend` start stages. Named stages provide filesystem and configuration at the end of their authored body.

| Start | Initial state |
| --- | --- |
| `from "image"` | Selected external image, local image, or named-stage state. |
| `from "scratch"` | Empty filesystem and image configuration. |
| `package as="payload"` | Empty filesystem and image configuration, saved as an immutable package. |
| `extend as="configured"` | Current consuming image filesystem and configuration. |

Each extend root starts independently from the same caller state. A second extend does not inherit the first extend's changes; use `from "configured"` to continue them.

## Build phase

A component build executes packages reachable from the selected output and their producer dependencies. Producers cannot depend on extend, because no consuming filesystem exists yet. Package arguments are fixed at this phase. Publication retains the selected transformation and snapshots needed for invocation.

A local file reference performs this packaging automatically as part of the consuming build. Its definition, package sources, and nested local references use the consumer's build-context root. The resulting artifact follows the same invocation path as an explicitly built component; see [local component workflows](../guides/components.md#share-a-component-within-a-repository).

## Invocation phase

A component call starts the retained transformation against the caller's current state. Stored packages become input leaves; their producers do not rerun. A derived stage needed at invocation can still execute there. Local file inputs used at invocation belong to the caller's context.

The selected output must descend from extend through FROM links. COPY dependencies supply files but do not make a package an acceptable caller-replacing output.

A call returns the selected filesystem and image configuration. Caller argument declarations remain in scope; private component arguments do not leak into it. Returned ENV values win collisions until a later caller ARG declaration reapplies its own CLI override. Like a FROM boundary, an entrypoint after the call clears inherited CMD unless a later CMD supplies one.

FROM consumes inherited ONBUILD triggers before authored instructions. Extend does not consume the caller's stored triggers. See the [execution contract](../reference/execution.md) for deferred references and inherited trigger planning.
