# Share a local component

Use one component from two OCI image definitions. Both images receive the same packaged greeting and choose how to display it. Local components build as part of the consuming image; you do not need a component registry or a separate packaging command.

You need [Coopr](../getting-started/index.md), Podman, and access to Docker Hub. Run Coopr and Podman as the same user with the same storage settings.

Create this directory structure:

```{ .text .no-copy }
hello-components/
├── hello.coopr
├── numbered.coopr
└── components/
    └── greeting.coopr
```

Create the directories and enter the project:

```sh
mkdir -p hello-components/components
cd hello-components
```

Run the remaining commands from this directory.

## Write the component

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

The `builder` stage generates the greeting, and `package` captures the file. `extend` starts from the calling image and copies the packaged file into it. Both images reuse this packaging work through the build cache and choose their own runtime commands.

## Call it from two images

In `hello.coopr`, select a base image and print the greeting:

```kdl
from "docker.io/redhat/ubi9:latest" {
    // Package and apply the shared component.
    component "./components/greeting.coopr"

    // Print the greeting when the container starts.
    cmd {
        exec "cat" "/hello.txt"
    }
}
```

In `numbered.coopr`, apply the same component and print the greeting with a line number:

```kdl
from "docker.io/redhat/ubi9:latest" {
    // Reuse the same component in another image.
    component "./components/greeting.coopr"

    // Print the greeting with a line number.
    cmd {
        exec "cat" "-n" "/hello.txt"
    }
}
```

The `./` prefix makes the reference a local file path. Both builds use `hello-components` as their context, so `./components/greeting.coopr` resolves to the same file. Local `copy` sources also resolve from this context root; copies from named stages use that stage's filesystem.

A local call uses the component's last top-level extend by default, even if FROM or PACKAGE stages follow it. If a shared file has several named extends, [append `#NAME` to its path](../guides/components.md#select-outputs-and-arguments) to choose one.

## Build and run

```sh
coopr build --file hello.coopr --tag hello:latest .
coopr build --file numbered.coopr --tag numbered:latest .
podman run --rm localhost/hello:latest
podman run --rm localhost/numbered:latest
```

Expected output from the two containers:

```{ .text .no-copy }
Hello, world!
     1	Hello, world!
```

## Update both images

Change the greeting in `components/greeting.coopr`, then rerun both build commands. Each image receives the change when rebuilt; previously built images keep their original greeting.

Changing a consumer's `cmd` changes how that container displays the greeting. It does not require repackaging the unchanged component.

## Share outside this repository

The [packaged-component tutorial](reusable-components.md) shows how to build OCI components separately and apply two of them to an image. The [component guide](../guides/components.md#build-an-oci-component) explains publishing to a registry.

For a checked-in example that shares files between two images, see [local components](https://github.com/strio-dev/coopr/tree/main/examples/local-components).
