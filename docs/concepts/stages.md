# Stages and component phases

A stage is a sequence of instructions operating on a filesystem and image configuration. `from`, `package`, and `extend` choose its initial state. A named stage refers to the state at the end of its body.

| Start | Initial state |
| --- | --- |
| `from "image"` | Selected external image, local image, or named-stage state. |
| `from "scratch"` | Empty filesystem and image configuration. |
| `package as="payload"` | Empty filesystem and image configuration, saved as an immutable package. |
| `extend as="configured"` | Current consuming image filesystem and configuration. |

Each `extend` starts from the same caller state. To continue another extend stage’s changes, use `from "configured"`.

COPY/ADD and RUN mount sources create dependencies without changing a stage's base. A generated external image needs an explicit ordering edge: `from "oci:out:latest" after="producer"` waits for the earlier `producer` stage before reading the context's `out` layout. `after` establishes order, not filesystem inheritance. It accepts one earlier stage name or numeric index.

## Build phase

Packaging happens before the component has a caller. It executes the packages needed by the selected output and their producer stages, fixes package arguments, and stores the snapshots alongside the transformation. Producers cannot depend on `extend`: the consuming filesystem is not available yet.

`coopr component build` packages an OCI component explicitly. A [local file reference](../guides/components.md#share-a-component-within-a-repository) packages it during the consuming build, using the consumer’s context root for definitions and package sources. Both produce an artifact for invocation against a consuming image.

## Invocation phase

Invocation starts the retained transformation from the caller’s current state. Stored packages supply their snapshots without rerunning producers. Other derived stages needed by the transformation execute during invocation. Context COPY instructions read the caller’s context.

A component must declare `extend`, but its selected output may start from an independent `from`. That output replaces the caller’s whole image, so caller-only files and configuration disappear. When no `extend` root is selected, the nearest preceding `extend` supplies the caller compatibility contract without executing its unused body. Unrelated dormant targets do not add requirements to a selected caller root. A `package` stage cannot itself be the selected output.

The selected output replaces the caller’s image with its filesystem, configuration and existing layers intact. An output descended from `extend` retains the caller’s base layers; an independent output inherits its selected base’s configuration and layers. Caller arguments stay in scope; component arguments stay private. Returned ENV wins collisions until a later caller ARG reapplies its CLI override. As after FROM, a new entrypoint clears inherited CMD; a later CMD restores a default.

FROM consumes inherited ONBUILD triggers before authored instructions. Extend does not consume the caller's stored triggers. See the [execution reference](../reference/execution.md) for deferred references and inherited trigger planning.

## Group instructions into a layer

Ordinary component instructions retain their individual layers. Use `layer` to publish several instructions’ net filesystem change as one layer:

```kdl
extend
layer {
    run "dnf install -y jq"
    run "dnf clean all"
}
```

The instructions still run in order, and ARG/ENV changes stay in scope for later instructions. An outer group absorbs nested groups; configuration-only or empty groups add no filesystem layer. See [layer groups](../reference/definition.md#layer-groups) for component calls and the image-lineage rule.
