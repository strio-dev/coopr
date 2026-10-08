# Coopr

Coopr builds Linux container images from ordered KDL definitions. Reuse common steps and files through local component definitions or packaged OCI components.

## Install

```sh
curl -fsSL https://coopr.strio.dev/install.sh | sh
coopr --version
```

For other installation options, see the [installation guide](docs/getting-started/index.md).

## Build an image

Put an `index.html` beside this `image.coopr`:

```kdl
from "docker.io/redhat/ubi9:latest"
run "dnf install -y nginx && dnf clean all"
copy "index.html" "/usr/share/nginx/html/index.html"
expose "80/tcp"
cmd {
    exec "nginx" "-g" "daemon off;"
}
```

Build and run it with Podman:

```sh
coopr build image.coopr --tag hello:latest
podman run --rm -p 127.0.0.1:8080:80 localhost/hello:latest
```

Coopr and Podman share native image storage when run as the same user with the same storage settings. Follow the [first-image tutorial](docs/tutorials/first-image.md) for the complete example.

## Reuse build steps

Use `component "./components/shared.coopr"` to share a definition within a build context without a separate component build or upload. See [local component workflows](docs/guides/components.md#share-a-component-within-a-repository).

For packaged components, follow the [two-component tutorial](docs/tutorials/reusable-components.md). See [storage](docs/guides/storage.md) for image stores and transfers.

## Development

Development uses the pinned Nix environment. See [contributing](CONTRIBUTING.md) for source builds, tests, and documentation checks.

Coopr is licensed under [MIT License](LICENSE).
