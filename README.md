# Coopr

Coopr builds Linux container images from ordered KDL definitions. Reuse common steps and files through local component definitions or packaged OCI components.

Serve an `index.html` alongside this `image.coopr` definition:

```kdl
from "docker.io/redhat/ubi9:latest"
run "dnf install -y nginx && dnf clean all"
copy "index.html" "/usr/share/nginx/html/index.html"
expose "80/tcp"
cmd {
    exec "nginx" "-g" "daemon off;"
}
```

Install the latest release binary and build the image:

```sh
curl -fsSL https://coopr.strio.dev/install.sh | sh
export PATH="$HOME/.local/bin:$PATH"
coopr build image.coopr --tag hello:latest
```

Rootless execution requires Linux user and mount namespaces, subordinate UID/GID ranges, and a compatible storage driver and OCI runtime. Foreign-architecture RUNs require host emulation. Start with [installation and requirements](docs/getting-started/index.md), then [build an image](docs/tutorials/first-image.md) and [reuse components](docs/tutorials/reusable-components.md).

Use `component "./components/shared.coopr"` to share a definition within a build context without a separate component build or upload. See [local component workflows](docs/guides/components.md#share-a-component-within-a-repository).

Images use Coopr's native local graph by default; Podman's graph is selectable. Components stay in a separate OCI store. Registry, OCI archive, Podman, and Docker copies use explicit destinations. See [storage](docs/guides/storage.md) and [security and limitations](docs/guides/security.md).

## Development

Development uses the pinned Nix environment. See [contributing](CONTRIBUTING.md) for source builds, tests, and documentation checks.

Coopr is licensed under [MIT License](LICENSE).
