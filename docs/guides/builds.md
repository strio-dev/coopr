# Build images

Build a local OCI image, choose its inputs and stages, then save or publish the result. For a first build using a component, follow [Build hello world](../tutorials/first-image.md).

## Build and run

Create a file for the image to include:

```sh
printf 'Hello from Coopr!\n' > message.txt
```

Save this as `image.coopr` in the same directory:

```kdl
from "docker.io/redhat/ubi9:latest" {
    // Copy a file from the build context into the image.
    copy "message.txt" "/message.txt"

    // Print the file when the container starts.
    cmd {
        exec "cat" "/message.txt"
    }
}
```

Build and run it:

```sh
coopr build image.coopr --tag app:dev
podman run --rm localhost/app:dev
```

Expected output:

```{ .text .no-copy }
Hello from Coopr!
```

Coopr and Podman must run as the same user with the same storage settings. Coopr stores `app:dev` as `localhost/app:dev`, ready for Podman to run. The [published Coopr container](container.md) has its own store.

## Choose the build context

The context supplies files for local COPY/ADD instructions and component paths. Passing a definition directly uses its parent directory as the context. With `--file FILE`, the context defaults to the current directory, or you can supply it explicitly.

For example, if the definition is at `build/image.coopr` and `message.txt` is at the project root, build from the root with:

```sh
coopr build --file build/image.coopr --tag app:dev .
```

Here, `copy "message.txt"` still reads from the project root. Relative `--file` paths resolve from the working directory first, then inside the context if absent. Always supply the exact definition filename; Coopr does not choose one or append `.coopr`.

### Exclude files

Put a `.cooprignore` file at the context root to exclude files from local copies and mounts:

```text
.git
bin/
*.log
```

Coopr uses the first existing file in this order: `.cooprignore`, `.containerignore`, `.dockerignore`. Patterns support `!` negation; `--ignorefile PATH` selects another file. Paths and symlinks must stay inside the context. Stores and temporary build artifacts are also excluded.

Repeat `--file` to combine definitions in order with one context. Each file supplies complete stage blocks; instructions stay inside their own stage. See [build contexts and local files](../reference/execution.md#build-contexts-and-local-files) for archive, Git, HTTP and stdin inputs.

## Use build stages

In a Go project with `go.mod` and `cmd/app`, save this as `build.coopr` at the project root:

```kdl
// Compile the application with the Go toolchain.
from "docker.io/library/golang:1.26" as="build" {
    workdir "/src"
    copy "." "."
    run "CGO_ENABLED=0 go build -o /app ./cmd/app"
}

// Copy only the executable into the runtime image.
from "scratch" as="runtime" {
    copy "/app" "/app" from="build"
    entrypoint {
        exec "/app"
    }
}
```

Build the final image, or select the build stage to keep the toolchain for debugging:

```sh
coopr build build.coopr --tag app:dev
coopr build build.coopr --target build --tag app:build
```

The last top-level stage is the default output. `--target` selects another stage; unrelated stages are skipped by default. The runtime image contains the executable without the compiler or source files.

Use `scratch` for a self-contained executable. Choose a suitable runtime base when the program needs shared libraries, certificates or other operating-system files.

## Read named inputs

For an existing file at `../shared/settings.conf`, expose its directory as a named input. Make a copy of `image.coopr` named `shared.coopr` and add this instruction inside its `from` block:

```kdl
copy "settings.conf" "/etc/app/settings.conf" from="shared"
```

Supply the directory when building:

```sh
coopr build shared.coopr --build-context shared=../shared --tag app:shared
```

This reads `../shared/settings.conf`. Named inputs also accept images (`docker-image://REFERENCE`), OCI layouts (`oci-layout://PATH:TAG`), Git and HTTP tar inputs. A name matching a FROM stage's alias replaces that stage's body; EXTEND and PACKAGE stages are not replaced. Numeric references still select stages. See [named contexts](../reference/execution.md#named-contexts).

FROM accepts native [image transports](../reference/execution.md#image-selection). When a stage generates a layout or archive for a later FROM, `after="producer"` makes that FROM wait for the named producer. Generated context files use a writable primary-context mount; see [RUN mounts](../reference/execution.md#run-mounts) for their lifetime and build-wide `--mount`.

## Build multiple platforms

```sh
coopr build image.coopr --platform linux/amd64,linux/arm64 --tag app:multi
```

The result is a local OCI index with both platform images. `--format docker` produces a Docker manifest list instead. Only Linux platforms are supported; RUN commands that execute foreign-architecture binaries need [host emulation](../reference/execution.md#platforms).

## Export an image or its files

Save the multi-platform image as an OCI archive:

```sh
coopr save --format oci-archive --output app.oci.tar app:multi
```

To export the final filesystem instead, choose a directory or tar output during the build:

```sh
coopr build image.coopr --output type=local,dest=output
coopr build image.coopr --output type=tar,dest=rootfs.tar
```

Filesystem outputs contain files, without the image configuration or layers of an image archive. For registries and Docker Engine destinations, see [tags and transfers](storage.md#tags-and-transfers). Image format and destination are separate: `--format docker` changes manifest/config format, while `docker-daemon:` selects Docker Engine.

`--compression-format gzip|zstd|zstd:chunked` controls exported layer compression and portable instruction-cache representation, including cache hits. See [output compression](../reference/execution.md#output-compression) for format compatibility and force-compression behavior.

## Refresh inputs

```sh
coopr build image.coopr --pull --tag app:dev
coopr build image.coopr --pull=never --tag app:dev
```

The default `missing` policy pulls a base only when needed. `--pull` selects `always`; `--pull=never` requires local images. `--pull=newer` compares registry content and permits local reuse if the registry request fails.

Component names resolve locally first. Refresh a stored component with `coopr component pull REFERENCE --tag NAME`. Pin image and component references by digest when builds must select fixed content, including external inputs inside components. See [caching](caching.md) for instruction-cache controls and `coopr build --help` for the full option list.
