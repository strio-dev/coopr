# Stages and component phases

A stage is a sequence of instructions operating on a filesystem and image configuration. `from`, `package`, and `extend` choose its initial state. A named stage refers to the state at the end of its body.

| Start | Initial state |
| --- | --- |
| `from "image"` | Selected external image, local image, or named-stage state. |
| `from "scratch"` | Empty filesystem and image configuration. |
| `package as="payload"` | Empty filesystem and image configuration, saved as an immutable package. |
| `extend as="configured"` | Current consuming image filesystem and configuration. |

Each `extend` starts from the same caller state. To continue another extend stage’s changes, use `from "configured"`.

## Build phase

Packaging happens before the component has a caller. It executes the packages needed by the selected output and their producer stages, fixes package arguments, and stores the snapshots alongside the transformation. Producers cannot depend on `extend`: the consuming filesystem is not available yet.

`coopr component build` packages an OCI component explicitly. A [local file reference](../guides/components.md#share-a-component-within-a-repository) packages it during the consuming build, using the consumer’s context root for definitions and package sources. Both produce an artifact for invocation against a consuming image.

## Invocation phase

Invocation starts the retained transformation from the caller’s current state. Stored packages supply their snapshots without rerunning producers. Other derived stages needed by the transformation execute during invocation. Context COPY instructions read the caller’s context.

The selected output must descend from extend through FROM links. COPY dependencies supply files but do not make a package an acceptable caller-replacing output.

The selected output replaces the caller’s filesystem and configuration. Caller arguments stay in scope; component arguments stay private. Returned ENV wins collisions until a later caller ARG reapplies its CLI override. As after FROM, a new entrypoint clears inherited CMD; a later CMD restores a default.

FROM consumes inherited ONBUILD triggers before authored instructions. Extend does not consume the caller's stored triggers. See the [execution reference](../reference/execution.md) for deferred references and inherited trigger planning.
