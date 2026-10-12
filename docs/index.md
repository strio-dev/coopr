# Build images. Reuse the parts.

<div class="coopr-intro" markdown>
Coopr builds OCI container images from ordered `.coopr` definitions.
Reuse common build steps and files as components from your repository or an OCI registry.
</div>

[Get started](getting-started/index.md){ .md-button .md-button--primary }
[Build hello world](tutorials/first-image.md){ .md-button }
{ .coopr-actions }

## Install Coopr

```sh
curl -fsSL https://coopr.strio.dev/install.sh | sh
coopr --version
```

See [installation](getting-started/index.md) for requirements, release binaries, the Coopr container, and Nix.

<div class="coopr-paths" markdown>
<div class="coopr-path" markdown>
<p class="coopr-path__title">Start with an image</p>

Write a definition with familiar `from`, `run`, and `copy` steps. Build and tag the image locally.

[First-image tutorial](tutorials/first-image.md)
</div>
<div class="coopr-path" markdown>
<p class="coopr-path__title">Share a component</p>

Share build steps and packaged files between images. Start with a local component, then package it for an OCI registry.

[Local-component tutorial](tutorials/local-components.md)
</div>
</div>

## Define once, reuse

Create a greeting during packaging, then copy it into each image that uses the component. Save this as `components/greeting.coopr`:

```kdl
// Build this stage when packaging the component.
from "docker.io/redhat/ubi9:latest" as="builder" {
    run "echo 'Hello, world!' > /hello.txt"
}

// Keep the greeting in the packaged component.
package as="greeting" {
    copy "/hello.txt" "/hello.txt" from="builder"
}

// Start from the calling image when the component is used.
// Copy the saved greeting without rerunning the builder.
extend {
    copy "/hello.txt" "/hello.txt" from="greeting"
}
```

`package` captures the generated file. `extend` starts from the calling image and applies the instructions inside its block. Use the component in `image.coopr`:

```kdl
from "docker.io/redhat/ubi9:latest" {
    // Package and apply the local component.
    component "./components/greeting.coopr"

    // Print the greeting when the container starts.
    cmd {
        exec "cat" "/hello.txt"
    }
}
```

Build and run the image with Coopr and Podman:

```sh
coopr build image.coopr --tag hello:latest
podman run --rm localhost/hello:latest
```

```{ .text .no-copy }
Hello, world!
```

No separate component build or upload is needed. [Follow the local-component tutorial](tutorials/local-components.md), or [package a component for registry reuse](tutorials/reusable-components.md).

## Build the way your project needs

- **Local and OCI components.** Use a file while developing, then package its transformations and files together for other repositories. [Reuse components](guides/components.md).
- **Ordered, familiar instructions.** Use `from`, `run`, `copy`, named stages, and parameterized arguments. [Definition syntax](reference/definition.md).
- **Instruction and package caches.** Reuse unchanged build steps, cache dependency downloads, and share caches locally or through a registry. [Caching](guides/caching.md).
- **Control your layers.** Keep ordinary instruction layers or group several steps into one net filesystem change with `layer { ... }`. [Layer groups](reference/definition.md#layer-groups).
- **Rootless builds and native storage.** Build on Linux without a builder daemon. Images are available to Podman when both tools use the same user and storage settings. [Storage](guides/storage.md) · [Host requirements](guides/security.md).
