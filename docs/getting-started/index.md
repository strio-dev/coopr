# Getting started

Coopr runs on Linux. Install the CLI, then follow the first-image tutorial.

## Install

```sh
curl -fsSL https://coopr.strio.dev/install.sh | sh
coopr --version
```

Other options include [release binaries](https://github.com/strio-dev/coopr/releases), the [container image](#run-the-published-container), and [Nix](#build-from-source-with-nix).

## Requirements

- Linux with user and mount namespaces enabled. Rootless builds need subordinate UID/GID ranges and working `newuidmap`/`newgidmap` helpers.
- An OCI runtime such as `crun` and the storage and networking helpers required by your container configuration. The release binary does not install these host tools.
- Registry access for uncached images and network access for package installation steps.

See [rootless setup](../guides/security.md#native-rootless-requirements) for host requirements and [security](../guides/security.md) for nested-container profiles and limitations.

## Build an image

[Build your first image](../tutorials/first-image.md) builds an NGINX image and runs it with Podman. The [component tutorial](../tutorials/reusable-components.md) builds a separate UBI9 image with shared settings and a policy check.

## Run the published container

Run Coopr from GHCR:

```sh
podman run --rm ghcr.io/strio-dev/coopr:latest --help
```

Use a release tag in place of `latest` to select an exact version. From the repository checkout described in the [first-image tutorial](../tutorials/first-image.md), build with rootless Podman:

```sh
podman run --rm \
  --device=/dev/fuse:rw \
  --security-opt=seccomp=unconfined \
  --security-opt=label=disable \
  -v "$PWD:/work:rw" \
  -v coopr-state:/var/lib \
  ghcr.io/strio-dev/coopr:latest \
  build /work/examples/first-image/image.coopr --tag coopr-demo:base
```

The `coopr-state` volume keeps images, components, and caches between runs. The image defaults to chroot isolation; RUNs share the outer container's network and other namespaces. See the [nested container profile](../guides/security.md#nested-container-profile) for isolation controls and limitations.

## Other installation options

To pin a release, pass its unprefixed version tag:

```sh
curl -fsSL https://coopr.strio.dev/install.sh | sh -s -- 1.2.3
```

You can download the [installer](../install.sh) or binary archives from [GitHub releases](https://github.com/strio-dev/coopr/releases). Each release includes [corresponding sources and rebuild instructions](../contributing.md#corresponding-source) in `coopr-sources.tar.gz`.

For an unprivileged installation, choose a writable prefix:

```sh
curl -fsSL https://coopr.strio.dev/install.sh | COOPR_INSTALL_PREFIX="$HOME/.local" sh
"$HOME/.local/bin/coopr" --version
```

## Build from source with Nix

From a checkout, with Nix flakes enabled:

```sh
nix build path:.#coopr -o result-coopr
./result-coopr/bin/coopr --version
```

For the development shell and tests, see [Contributing](../contributing.md).
