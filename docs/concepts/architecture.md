# Architecture

Coopr builds OCI container images from KDL definitions. Instructions run in order inside stage blocks; components let you reuse those instructions and package the files they need.

Builds run locally through embedded Buildah, without a builder daemon. Coopr calls Buildah's Go APIs directly. Runtime, storage, network, and remote-input helpers are still needed; see [rootless requirements](../guides/security.md#native-rootless-requirements).

## From definition to image

1. **Read the inputs.** Coopr reads the definition and context, validates instructions, and applies the requested target, platform, and build arguments.
2. **Plan the required stages.** Stage references in FROM, COPY/ADD, and RUN mounts establish dependencies. Coopr plans the stages needed by the selected output and resolves their external image metadata as needed.
3. **Execute ready work.** Buildah applies filesystem and image-configuration changes. Instructions stay ordered within each stage; independent stages can run concurrently within the `--jobs` budget. Component calls resolve and prepare their artifacts when the required stage needs them.
4. **Keep the result.** Coopr retains the image locally, assigns requested tags, and performs any requested exports or transfers.

Unused stages are normally skipped. Inherited ONBUILD instructions or a component's returned configuration can introduce dependencies that require further planning. The planner rejects cycles and ambiguous references. See [stages and component phases](stages.md) for dependency and output rules.

## What a component carries

A packaged component is an OCI artifact containing its retained definition, selected output, fixed package arguments, platform, and immutable package snapshots. Each package includes its filesystem and image configuration. Consumers can use those files without the publisher's checkout.

Packaging runs the required producer and package stages. Invocation uses the saved packages and runs the selected `extend` against a consuming image. A local component path goes through the same packaging and invocation phases automatically during the build. See [package components](../guides/components.md).

Descriptor sizes and digests are verified. Pinning a component fixes that artifact's content; mutable image or component references inside it can still change. See [trust images and components](../guides/security.md#trust-images-and-components).

## Image state and layers

A stage holds a filesystem, image configuration, target platform, and argument scope. Coopr preserves configuration fields it does not change. A component returns its selected filesystem, layers, and configuration together, including when its output replaces the caller with an independent base.

By default, filesystem-changing instructions retain their layers; configuration-only instructions add no filesystem layer. Components do not compact their changes automatically. An authored [`layer` block](../reference/definition.md#layer-groups) combines its net filesystem changes into one layer.

## Where build state lives

Pulled images, build results, and local instruction-cache snapshots use native container storage. Podman and Buildah can use the same images when they run as the same user with matching storage settings. Packaged components have a separate OCI artifact store. See [image and component storage](../guides/storage.md).

Caches can restore intermediate image state so later instructions can continue from it. Portable caches can share results through a local directory or registry. Cache restoration preserves the selected image's layer chain, configuration, and root metadata together. See [caching](../guides/caching.md) for reuse and refresh rules.
