# Package components

Components apply shared changes to images. Their `extend` stage starts from the consuming image. Use a local definition directly, or package it as an OCI component to share outside your checkout.

## Share a component within a repository

Use a file path when the component lives in your build context:

```kdl
from "docker.io/redhat/ubi9:latest"
component "./components/shared.coopr" channel="api"
```

Coopr packages the required stages and applies the selected transformation during the consuming build. Package results use the existing build cache.

The checked-in `examples/local-components/build` example supplies shared settings to two images:

```text
local-components/
└── build/
    ├── api.coopr
    ├── worker.coopr
    └── components/
        ├── shared.coopr
        └── settings.conf
```

Build both with the same context:

```sh
cd examples/local-components/build
coopr build -f api.coopr . --tag coopr-local:api
coopr build -f worker.coopr . --tag coopr-local:worker
```

Definition paths, package COPY sources, and nested local references resolve from the context root, using the consumer’s ignore policy. Here, `./components/shared.coopr` packages `components/settings.conf`. A definition’s subdirectory does not change that root. Invocation-time COPY also reads the caller’s context.

Local paths must start with `./`, `../`, or `/`. Thus `team/shared.coopr` is a registry reference; `./team/shared.coopr` is a context file. Paths stay confined to the context: `/` means its root and `../` cannot escape it. A glob must match exactly one definition.

Properties such as `channel="api"` supply component arguments. See [outputs and arguments](#select-outputs-and-arguments) for when their values become fixed.

## Build an OCI component

Build once, then invoke the selected output by local tag or registry reference:

```sh
coopr component build ./components/settings/component.coopr --tag settings
coopr component save --output settings.oci.tar settings
coopr component push settings registry.example.com/team/settings:1
```

The registry command publishes the component. Invoke the local tag as `local:settings`; registry references never fall back to local tags.

Pull a published component into a local name or load an archive:

```sh
coopr component pull registry.example.com/team/settings:1 --tag settings
coopr component load --input settings.oci.tar
```

Both commands retain all packaged platforms. Loading restores the archived name; `--tag NAME` chooses another. A pull without `--tag` prints the stored digest, which can be used directly in a component instruction.

## Capture component-owned files

Capture owned files in a `package` stage or its producers, then copy from that package after `extend`. OCI invocation cannot read the publisher’s checkout. An ordinary context COPY reads the consumer’s context.

Packages start empty. `from "package-name"` inherits their filesystem and configuration; `copy ... from="package-name"` copies only files. Producers cannot depend on `extend`, because packaging has no caller.

## Select outputs and arguments

`--target NAME` selects the component’s output stage. `extend` remains mandatory, but a later output can have an independent FROM base. A package cannot itself be the output. Publication fixes it; invocation has no target selector. Publish other outputs under separate references.

Properties on a component call are string arguments, including a property named `target`. Arguments in package-producer scope become fixed during component build. For local references, different property values can package another variant. Declare invocation arguments after `extend`. The [two-component tutorial](../tutorials/reusable-components.md) demonstrates this boundary.

## Preserve layers or group changes

A component returns its selected image’s layers and configuration. It does not automatically compact its instructions. Group steps inside the component when intermediate files should disappear from the published layer:

```kdl
extend
layer {
    run "dnf install -y jq"
    run "dnf clean all"
}
```

You can also group a call with instructions in its consumer. With the shared component from the repository example above:

```kdl
from "docker.io/redhat/ubi9:latest"
layer {
    component "./components/shared.coopr" channel="api"
    run "dnf install -y jq"
    run "dnf clean all"
}
```

The group publishes one net filesystem layer above the starting image. Nested groups are absorbed by the outer group. A component that returns an independent base cannot run inside a group: replacing the image would break the group’s starting lineage.

## Replace the caller image

A component may select an independently based output after its mandatory `extend`:

```kdl
extend distro="rhel"
from "docker.io/redhat/ubi9:latest" as="runtime"
run "dnf install -y jq && dnf clean all"
```

Invoking this output replaces the caller with the selected UBI image and the authored changes. UBI’s normal configuration is inherited; caller-only files and settings are discarded. The `extend` requirement is checked against the caller even though that stage is not used as the output’s filesystem base.

## Declare compatibility

```kdl
extend as="configured" {
    distro "fedora" "rhel"
    package-manager "dnf" "yum"
    architecture "amd64" "arm64"
}
```

Coopr checks the caller before execution. Any listed value may match within a field; every declared field on a selected extend root must match. An independently based output with no selected extend root checks the nearest preceding extend’s contract. `distro-version` requires exact versions and `distro`. Ranges and inferred family matches are unsupported.

## Share across platforms

`coopr component build FILE --platform linux/amd64,linux/arm64` packages one complete artifact per platform into an index. Invocation selects the child matching the consumer's platform. A digest identifies bytes, not publisher trust; review the [security guide](security.md) before using third-party transformations.
