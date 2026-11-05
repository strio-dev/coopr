# Coopr

Coopr builds OCI container images from ordered KDL definitions. Share build steps and files through reusable components, using local definitions or packaged OCI artifacts.

## Install

```sh
curl -fsSL https://coopr.strio.dev/install.sh | sh
coopr --version
```

See [getting started](docs/getting-started/index.md) for Linux prerequisites and other installation options.

## Hello, world

Save this as `components/greeting.coopr`:

```kdl
from "docker.io/redhat/ubi9:latest" as="builder" {
    run "echo 'Hello, world!' > /hello.txt"
}
package as="greeting" {
    copy "/hello.txt" "/hello.txt" from="builder"
}
extend {
    copy "/hello.txt" "/hello.txt" from="greeting"
}
```

The component generates a greeting, packages the file, and copies it into the calling image. Save the caller as `image.coopr`:

```kdl
from "docker.io/redhat/ubi9:latest" {
    component "./components/greeting.coopr"
    cmd {
        exec "cat" "/hello.txt"
    }
}
```

Build and run it with Podman:

```sh
coopr build --file image.coopr --tag hello:latest .
podman run --rm localhost/hello:latest
```

The container prints:

```text
Hello, world!
```

Run Coopr and Podman as the same user with the same storage settings. The [first-image tutorial](docs/tutorials/first-image.md) explains each step and shows how changes affect the cache.

## Reuse build steps

Apply a shared definition with `component "./components/shared.coopr"`. [Share a local component](docs/tutorials/local-components.md) between two images without a separate packaging command or registry.

For components you can distribute, follow the [reusable-components tutorial](docs/tutorials/reusable-components.md) to package two components and apply them to an image. Explore [the examples](examples/README.md) for cache mounts and published components, or read about [image storage and transfers](docs/guides/storage.md).

## Application integrations

Use the [Go execution library](docs/guides/library.md) to embed literal programs, or the [local Unix-socket API](docs/guides/api.md) to supervise a Coopr process with structured requests and results. The new API supports literal execution, native image/component builds and explicit artifact publication. It is an unreleased local implementation and is not present in published `0.5.0`; ordinary CLI operation remains daemonless.

The [Go service client](docs/guides/client.md) shares public API types and builds with CGO disabled. It keeps transport and cancellation handling in Coopr while the application owns the service process. The separate `examples/client` module demonstrates independent consumption; see the [Stoke migration plan](docs/development/stoke-client-migration.md) for the later consumer change and release requirements.

## Development

The development flake exposes `pkgs.coopr.buildImage`: Nix builds packages and
closure-complete image inputs, then `nix run` invokes ordinary Coopr outside
the build sandbox to compose components. See the [Nix guide](docs/guides/nix.md)
and [Nix examples](examples/nix/README.md) for the unreleased interface.

Development uses the pinned Nix environment. See [contributing](CONTRIBUTING.md) for source builds, tests, and documentation checks.

Coopr is licensed under the [MIT License](LICENSE).
