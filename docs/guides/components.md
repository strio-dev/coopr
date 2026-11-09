# Package components

Share build steps, image settings, and files through a component. Use a local definition within a repository, or package it as an OCI artifact that other repositories can consume. For a first example, follow [Share a local component](../tutorials/local-components.md).

## Share a component within a repository

The [checked-in local-components example](https://github.com/strio-dev/coopr/tree/main/examples/local-components) shares settings between two images. Its `api.coopr` selects a base and applies the component with an argument:

```kdl
from "docker.io/redhat/ubi9:latest" {
    // Package and apply the shared settings for this image.
    component "./components/shared.coopr" {
        arg "channel" "api"
    }
}
```

The component and its input file live under the shared build context:

```{ .text .no-copy }
local-components/
└── build/
    ├── api.coopr
    ├── worker.coopr
    └── components/
        ├── shared.coopr
        └── settings.conf
```

From a repository checkout, build both with the same context:

```sh
cd examples/local-components/build
coopr build -f api.coopr . --tag coopr-local:api
coopr build -f worker.coopr . --tag coopr-local:worker
```

Each build packages the required component stages as needed, reusing cached results, then applies the component to its current image. The call’s `arg "channel" "api"` supplies an argument; the worker definition supplies `arg "channel" "worker"`.

Local component definitions, package COPY sources, and nested local references use the consumer’s context root and ignore policy. Here, that root is `build/`, so the component reads `components/settings.conf`. Putting a definition in a subdirectory does not change its context. COPY inside `extend` also reads the caller’s context unless it names a stage or another input.

Prefix local references with `./`, `../`, or `/`: `./team/shared.coopr` is a file, while `team/shared.coopr` is a registry reference. Paths follow [context confinement rules](../reference/execution.md#build-contexts-and-local-files): `/` means the context root, `../` cannot escape it, and a glob must match exactly one definition.

## Capture component-owned files

The shared component captures its file in a `package` stage, then copies from that package inside `extend`. Its `components/shared.coopr` contains:

```kdl
// Capture the file while packaging, before there is a caller.
package as="settings" {
    copy "components/settings.conf" "/settings.conf"
}

// Apply the saved file and caller-selected settings to the image.
extend as="configured" {
    arg "channel" "stable"
    env coopr_channel="${channel}"
    copy "/settings.conf" "/etc/coopr/settings.conf" from="settings"
    run "test -s /etc/coopr/settings.conf"
}
```

A packaged component carries the captured file with it. Consumers do not need the publisher’s checkout. A context COPY inside `extend` would instead read a file supplied by the consumer.

Packages start empty. Inside `extend`, `from "settings" { ... }` inherits the package’s filesystem and configuration; `copy ... from="settings"` copies only files. Stages that prepare a package’s contents cannot depend on caller state: packaging has no caller. An extend can also consume a FROM directly; that dependency runs at call time with the consumer’s context. See [component phases](../concepts/stages.md) for how Coopr separates this work.

## Build an OCI component

From the same `examples/local-components/build` directory, package the shared component separately:

```sh
coopr component build --file components/shared.coopr --tag shared .
```

`--file` selects the definition and `.` keeps the context at `build/`, just as in the local invocation. Passing only `components/shared.coopr` would use `components/` as the context and change where its COPY sources resolve.

Invoke the stored artifact with the same argument binding:

```kdl
component "shared" {
    arg "channel" "api"
}
```

The package stays fixed until you rebuild it; changing the call’s `channel` changes the consumer without repackaging the settings file.

The artifact also carries FROM recipes, including unused stages and stages used only to produce a package. Their instructions, defaults, and path strings are readable by artifact consumers and add to artifact size. Review [component source disclosure](security.md#component-source-disclosure) before sharing it. PACKAGE producer instructions are removed.

Older component formats are rejected. Rebuild those artifacts from source rather than trying to migrate them.

### Save and load an archive

```sh
coopr component save --output shared.oci.tar shared
coopr component load --input shared.oci.tar
```

The archive includes all packaged platforms. Loading restores its name; `--tag NAME` chooses another.

### Push and pull from a registry

Replace `registry.example.com/team/shared:1` with a registry reference you can write to and [log in](storage.md#tags-and-transfers) if required:

```sh
coopr component push shared registry.example.com/team/shared:1
coopr component pull registry.example.com/team/shared:1 --tag shared
```

Use that registry reference in a `component` instruction to consume it elsewhere. Coopr checks its component store first, then resolves a missing name from the registry. Names without a tag use `latest`; a bare `sha256:` digest selects an artifact already in the local store.

Pulling retains all packaged platforms. Without `--tag`, pull prints the stored digest, which you can use directly in a component instruction. Pin references by digest when consumers must select fixed content; review the [security guide](security.md) before using third-party components.

### Capture dependencies with Bale

Pushing a component also publishes a [Bale dependency declaration](https://github.com/strio-dev/bale/blob/v0.1.3/docs/dependencies.md) for registry references discovered in its default selected invocation dependencies. This includes nested component calls and external image and [artifact file inputs](../reference/definition.md#copy-oci-artifact-files). Unselected retained recipes are not inventoried. Dependencies used only to produce an embedded package are already captured in that package and are excluded. Built images do not receive links to their build components.

Coopr publishes the attachment through OCI referrers when the registry supports them, or the standard referrers-tag fallback otherwise. The registry credentials need permission to read and write those artifacts.

Component registry authentication keeps [ORAS 2.7’s upstream protections](https://github.com/oras-project/oras-go/releases/tag/v2.7.0). An HTTPS registry must use an HTTPS token service. Token services on a different host cannot resolve to loopback, link-local, or cloud metadata addresses; ordinary private LAN token services are supported.

Dependency tags remain tags when the declaration is published; Coopr does not fetch those dependencies. When Bale captures the parent component, it resolves the declared selectors, follows their declarations recursively, and records immutable digests. Locked transfers use those captured digests; a latest refresh resolves the selectors again.

Discovery includes literal references in the default selected graph and references resolved before any unknown component return. Coopr reports incomplete discovery when selection depends on caller values, dynamic FROM/COPY/mount or `after` selectors, an unknown mount type, or unfetched external image configuration and ONBUILD triggers.

Mutable selectors are not a guarantee about future dependencies. An argument can activate a retained recipe whose dependencies were not declared. Include the additional artifacts required by your invocation explicitly in the Bale recipe. A registry declaration cannot include a context-local component file or an unnamed local digest.

The declaration is a separate OCI attachment: it does not change the component's manifest or payload. When Bale delivers a component, Coopr reads its destination records and uses the captured destinations for declared dependencies. Nested components use their own records from the same bundle capture. The original registry is no longer needed for those captured inputs.

Component pull, tag, save and load preserve the delivery records. A bundle does not guarantee every future argument-selected branch is available offline. For an offline build, include the dependencies of the actual invocation and make its components, images and artifact files available in their local stores. Coopr fixes the selected delivery for the build; workers do not refresh it halfway through execution. If several bundle captures or dependency policies could apply, Coopr reports the ambiguity rather than choosing one automatically.

## Select outputs and arguments

Use `--target NAME` to select a named output when building the component. For the shared component:

```sh
coopr component build --file components/shared.coopr --target configured --tag shared .
```

The selected output is fixed in the artifact; publish other outputs under separate references. For a local file, append `#NAME` to select an output during automatic packaging:

```kdl
from "docker.io/redhat/ubi9:latest" {
    component "./components/shared.coopr#configured" {
        arg "channel" "api"
    }
}
```

Without a selector, a local call uses the last top-level `extend`. Trailing `from` and `package` stages do not change that default. Select another named `extend` with `--target` when packaging or `#NAME` in a local call. Nested FROM stages are helpers within an extend, not callable targets. `#NAME` applies only to explicit local paths, not stored or registry references. Target names follow `--target` matching, including case-insensitive stage aliases.

A component call accepts only `arg` children, each with a name and an explicit value. These bindings belong to that invocation and do not change the caller’s argument scope. A binding named `target` is an ordinary component argument; use the local path’s `#NAME` suffix to select an output. Arguments used by package stages and their producers become fixed during component build. A local call can package another variant when those values change.

Declare arguments inside `extend` to let consumers choose their values, as `channel` does above. The [packaged-component tutorial](../tutorials/reusable-components.md) demonstrates changing an invocation argument without rebuilding the package.

Invocation arguments can also change a source selector, choosing another earlier stage, an external image, or `scratch` for FROM, provided the selected graph is valid. Each component resolves names and numeric positions in its own original stage namespace. Passing a caller-stage name as an argument does not import it. Use an explicit [named context](../reference/execution.md#named-contexts) for an external input.

For a static selected graph, publication removes FROM bodies proven unselectable. It preserves their names and original numeric slots, and keeps recipes that arguments, inherited configuration or named contexts could select. Dynamic source selection, ONBUILD and component-return paths conservatively retain full source. See [publication pruning and source disclosure](../reference/execution.md#component-build-and-invocation-phases).

Selecting a retained FROM recipe runs it with the caller's context and invocation inputs, even if it was only used to produce a package during publication. Selecting a package uses its saved filesystem and full configuration; packages never rebuild at invocation. Only packages required by the publication's selected graph are captured. Selecting an uncaptured package fails and asks you to rebuild the component with that package selected.

## Preserve layers or group changes

A component returns its selected image’s layers and configuration without automatic compaction. Use `layer` to combine ordered steps into their net filesystem change. For example, group package installation and cleanup inside a component:

```kdl
extend {
    // Keep the installed files without retaining downloaded package data.
    layer {
        run "dnf install -y jq"
        run "dnf clean all"
    }
}
```

You can also group a call with instructions in its consumer. With the shared component from the repository example above:

```kdl
from "docker.io/redhat/ubi9:latest" {
    // Combine the component's changes and package installation into one layer.
    layer {
        component "./components/shared.coopr" {
            arg "channel" "api"
        }
        run "dnf install -y jq"
        run "dnf clean all"
    }
}
```

A group with filesystem changes adds one layer above the starting image. Empty or configuration-only groups add none; an outer group absorbs nested groups. A component that returns an independent base cannot run inside a group, because it replaces that starting image. See [layer groups](../reference/definition.md#layer-groups) for the lineage rule.

## Replace the caller image

A component can replace the caller rather than modify its existing image. This example selects a fresh UBI9 base for its output:

```kdl
extend as="runtime" {
    // Return a fresh UBI image instead of the caller's filesystem.
    from "docker.io/redhat/ubi9:latest" {
        run "dnf install -y jq && dnf clean all"
    }
}
```

The named `runtime` extend returns UBI’s configuration and layers, plus the authored changes. Caller-only files and settings are discarded. This FROM runs during invocation because it supplies the selected extend’s result.

Direct instructions in `extend` always modify the caller. A nested FROM owns its own instructions. The last non-ARG child determines the returned image: a nested FROM returns that stage, while a direct instruction returns the caller with its changes. To use a nested stage only as a helper, finish with a direct COPY from it.

## Share across platforms

Component builds use `--platform`, defaulting to the host platform, just like image builds. Local file calls package for the calling stage’s platform; published calls select the matching child from their OCI index.

Build one component index for multiple Linux platforms:

```sh
coopr component build --file components/shared.coopr --platform linux/amd64,linux/arm64 --tag shared:multi .
```

Each child contains the complete component for its platform. Invocation selects the child matching the consumer’s platform; save, push, pull and load retain the index. If package producers execute foreign-architecture binaries, the host needs [emulation](../reference/execution.md#platforms). This settings-only package copies a file without running such binaries.
