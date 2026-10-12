# Stages and component phases

A stage groups ordered instructions around an initial image state. `from`, `package`, and `extend` each own their instructions inside `{ ... }`. Name a stage with `as="NAME"` to refer to its completed result.

| Stage | Starts from |
| --- | --- |
| `from` | An image, a named stage or package, or an empty image with `"scratch"`. |
| `package` | An empty filesystem and image configuration. Its result is saved in the component artifact. |
| `extend` | The consuming image's current filesystem and configuration. Required for every component definition. |

A standalone image definition uses FROM stages. A component adds packaging and invocation phases: prepare reusable inputs, then apply instructions to a caller.

## Packaging

Packaging follows the dependency graph. Coopr builds the packages needed by the selected output and the stages they depend on, then stores their files and image configuration in the artifact. A FROM stage runs during packaging only when a package needs it; its position or nesting does not determine its phase. Arguments used during package production are fixed at this point, including values exposed to RUN.

For example, save this as `components/greeting.coopr`:

```kdl
// Generate the file while packaging the component.
from "docker.io/redhat/ubi9:latest" as="builder" {
    run "echo 'Hello, world!' > /hello.txt"
}

// Capture the generated file in the artifact.
package as="greeting" {
    copy "/hello.txt" "/hello.txt" from="builder"
}

// Apply the saved file when an image calls the component.
extend as="configured" {
    copy "/hello.txt" "/hello.txt" from="greeting"
}
```

The `builder` produces the file; `greeting` captures it; `configured` defines how to use it later. Packages carry completed results between phases. A package cannot depend on caller state because there is no caller during packaging. An independently based FROM can supply a package, even when nested inside an extend.

`coopr component build` packages an artifact explicitly. A [local file reference](../guides/components.md#share-a-component-within-a-repository) performs packaging automatically during the consuming build, using the consumer's context root. Both paths reuse eligible cached work.

## Invocation

Save this caller as `image.coopr` beside the `components/` directory:

```kdl
from "docker.io/redhat/ubi9:latest" {
    // Package the local definition and invoke its selected extend.
    component "./components/greeting.coopr"

    // Print the file supplied by the component.
    cmd {
        exec "cat" "/hello.txt"
    }
}
```

The component uses its saved package and runs `configured` against the caller's current image. Stored artifacts do not rerun packages or packaging-only producers when invoked. Local references check their definition and inputs during each build and can reuse cached packages. Follow [Build hello world](../tutorials/first-image.md) to build and run this example.

Invocation follows the selected extend's dependencies and stops at saved package inputs. An extend can also reference a top-level FROM directly: that stage runs at call time with the consumer's context and invocation arguments. If a stage supplies both a package and the extend directly, it runs in both phases with each phase's inputs. Its saved package and arguments used during packaging remain fixed.

Context COPY in an invocation stage reads the caller's context unless it selects a package, stage, or other input with `from=`. Files belonging to a distributed component must be captured during packaging, as `/hello.txt` is above.

## Select the result

Images default to the last top-level FROM; components default to the last top-level EXTEND. Trailing FROM and PACKAGE stages do not change the component output and are skipped.

Use `--target NAME` when building an artifact, or append `#NAME` to a local component path, such as `./components/greeting.coopr#configured`. A stored or registry artifact keeps the output chosen when it was packaged. Nested FROM aliases can supply dependencies but are not callable component targets.

Each extend starts independently from the same caller state. To continue another extend's result, place `from "configured" { ... }` inside the new extend.

### Nested FROM and caller state

Direct instructions inside an extend modify its caller state. A nested FROM has its own image and instruction block. The last non-ARG child determines the result:

- A nested FROM returns that stage's completed image.
- A direct instruction returns the caller with its changes.
- ARG declarations alone do not switch the result.

To use a nested stage only as a helper, finish the extend with a direct COPY from that stage. To return an independently based image, finish with its FROM block. Only the selected result's dependencies execute; an unused caller-modifying branch can be skipped entirely.

Invocation returns the selected filesystem, configuration, and layers together. An independently based result inherits its own base's settings and drops caller-only files and configuration. After the call, the caller's argument scope is preserved; component arguments stay private. See [state and stages](../reference/execution.md#state-and-stages) for argument/ENV and command-inheritance rules.

## Dependencies and inherited instructions

A stage must be declared before a FROM, COPY/ADD, or RUN mount can consume it. Within an extend, nested FROM helpers become available to later instructions. COPY/ADD and RUN mounts with `from=` create dependencies without changing the destination stage's base. A generated image input can use [`after="producer"`](../reference/definition.md#instructions) to wait for its producer; this establishes order without inheriting its state.

FROM runs inherited ONBUILD triggers before authored instructions. EXTEND does not consume the caller's stored triggers. See [inherited ONBUILD](../reference/execution.md#inherited-onbuild) for context and dependency rules.

## Group instructions into a layer

Components retain filesystem instruction layers by default. Use `layer` to combine several instructions' net filesystem change into one layer:

```kdl
extend {
    layer {
        run "dnf install -y jq"
        run "dnf clean all"
    }
}
```

Instructions still run in order, and ARG/ENV changes remain in scope afterward. An outer group absorbs nested groups; empty or configuration-only groups add no filesystem layer. A group can include a component call that keeps the starting image's lineage, but rejects one that returns an independent base. See [layer groups](../reference/definition.md#layer-groups).
