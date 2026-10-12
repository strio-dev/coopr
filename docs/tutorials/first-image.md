# Build hello world with a component

Build an OCI container image that prints `Hello, world!`. A reusable component generates the greeting during packaging, then copies it into the calling image.

## Before you start

[Install Coopr](../getting-started/index.md) on Linux. You also need Podman to run the image and access to Docker Hub for the first base-image pull. Run Coopr and Podman as the same user with the same storage settings so they can use the same local images.

## Write the component

Start in an empty directory:

```sh
mkdir -p coopr-hello/components
cd coopr-hello
```

Save this as `components/greeting.coopr`:

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

The `builder` stage runs in UBI9 and generates `/hello.txt`. The `greeting` package captures that file. `extend` begins the portion applied to the calling image: it copies the packaged greeting into the caller's filesystem.

## Use it in an image

Save this as `image.coopr`:

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

`from` selects the image's base. `component` applies the local definition, and `cmd` sets the command to run when a container starts. This example uses the `cat` command already in UBI9.

The `./` prefix identifies a local component. Its path resolves inside the build context, which is the `coopr-hello` directory here.

## Build and run

```sh
coopr build --file image.coopr --tag hello:latest .
podman run --rm localhost/hello:latest
```

`--file` selects the definition, `.` supplies the build context, and `--tag` names the result. Coopr builds the local component's package as needed; you do not need a separate component-build command or registry.

The container prints this line and exits:

```{ .text .no-copy }
Hello, world!
```

Coopr stores the short tag `hello:latest` as `localhost/hello:latest`. Podman uses that name to run the image from the shared local store. See [image storage](../guides/storage.md) for storage configuration.

## Change the greeting

In `components/greeting.coopr`, replace the `run` instruction with:

```kdl
run "echo 'Hello again!' > /hello.txt"
```

Build and run again:

```sh
coopr build --file image.coopr --tag hello:latest .
podman run --rm localhost/hello:latest
```

Expected output:

```{ .text .no-copy }
Hello again!
```

Changing the greeting rebuilds the component's package and the image that uses it. Build once more without editing the files to reuse cached results. See [caching](../guides/caching.md) for cache controls and refresh rules.

Next, [share the local component](local-components.md) between two images, or [package components](reusable-components.md) for distribution. You can also build the [checked-in hello-world example](https://github.com/strio-dev/coopr/tree/main/examples/first-image).
