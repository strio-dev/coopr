# Package components

A definition containing `extend` is a component. Invoke a local definition directly, or build an OCI component to share outside your checkout.

## Share a component within a repository

Use a file path when the component lives in your build context:

```kdl
from "docker.io/library/ubuntu:24.04"
component "./components/shared.coopr" channel="api"
```

Coopr builds the required package stages locally, then applies the component's selected transformation to the caller's image. You do not need a separate `coopr component build` command or a registry. Package results use the existing build cache.

The definitions in `examples/local-components/build` supply shared settings to two images. The `build` directory is the build context:

```text
local-components/
└── build/
    ├── api.coopr
    ├── worker.coopr
    └── components/
        ├── shared.coopr
        └── settings.conf
```

Build both from that directory:

```sh
cd examples/local-components/build
coopr build -f api.coopr . --tag coopr-local:api
coopr build -f worker.coopr . --tag coopr-local:worker
```

Paths follow context COPY rules: `./components/shared.coopr` resolves from the selected build-context root. Its package uses `copy "components/settings.conf" "/settings.conf"` to read the file beside it. Package COPY sources and nested local component paths use that same root, even when a definition is in a subdirectory, and the consuming build's ignore policy applies. An invocation-time COPY continues to read the caller's context.

Local definitions require a leading `./`, `../`, or `/`, regardless of their file extension. Other names are OCI references: `team/shared.coopr` names a registry repository, while `./team/shared.coopr` selects a context file. The presence of a matching local file does not change registry resolution. Paths remain confined to the context; `/` means its root, and `../` does not grant access to its parent. A glob must select exactly one definition file.

Component properties supply arguments to local packaging and invocation. Package-scope arguments are fixed for that invocation's artifact; later invocations with different values can build another variant. Arguments declared after `extend` configure the caller transformation.

## Build an OCI component

Build once, then invoke the selected output by local tag or registry reference:

```sh
coopr component build ./components/settings/component.coopr --tag settings
coopr component copy settings oci-archive:settings.oci.tar
coopr component copy settings registry:registry.example.com/team/settings:1
```

The registry command publishes to your chosen repository. An unprefixed build tag names the local component; `local:settings` explicitly selects it at invocation. A registry reference does not fall back to a local tag.

## Capture component-owned files

Put files in a `package` stage or its producer stages, then copy from the stored package after `extend`. Invocation of an OCI component does not read files beside its original definition. An ordinary context COPY at invocation instead reads the consuming caller's context.

Packages begin with empty filesystem and image configuration. `from "package-name"` inherits both; `copy ... from="package-name"` copies files without replacing the caller's configuration. Producer stages cannot depend on `extend` because the caller is unavailable during packaging.

## Select outputs and arguments

`--target NAME` selects a component output when building it. The output must descend from `extend` through FROM links. Publication fixes that output; invocation has no target selector. Publish different outputs under different references.

Properties on a component call are string arguments, including a property named `target`. Arguments in package-producer scope become fixed during component build. Declare adjustable arguments after `extend` where possible. The [two-component tutorial](../tutorials/reusable-components.md) demonstrates this boundary.

## Declare compatibility

```kdl
extend as="configured" {
    distro "fedora" "rhel"
    package-manager "dnf" "yum"
    architecture "amd64" "arm64"
}
```

Any value in one field may match; every declared field and retained extend root must match. `distro-version` requires exact versions and a `distro` declaration. Version ranges and inferred distribution-family matches are unsupported. Compatibility checks inspect the caller image before executing the component.

## Share across platforms

`coopr component build FILE --platform linux/amd64,linux/arm64` packages one complete artifact per platform into an index. Invocation selects the child matching the consumer's platform. A digest identifies bytes, not publisher trust; review the [security guide](security.md) before using third-party transformations.
