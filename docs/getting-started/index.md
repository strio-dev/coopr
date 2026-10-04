# Getting started

Coopr builds Linux images and reusable OCI components. Install a release binary, then run the CLI on a Linux host.

## Install

```sh
curl -fsSL https://coopr.strio.dev/install.sh | sh
export PATH="$HOME/.local/bin:$PATH"
coopr --help
```

The installer selects the latest stable [GitHub release](https://github.com/strio-dev/coopr/releases), checks its SHA256 checksum, and installs the static amd64 or arm64 binary in `~/.local/bin`. It keeps the accompanying licenses in `~/.local/share/licenses/coopr`. It does not require Nix or change your shell configuration.

To install an exact release, pass its unprefixed tag:

```sh
curl -fsSL https://coopr.strio.dev/install.sh | sh -s -- 1.2.3
```

Set `COOPR_INSTALL_PREFIX` on the shell command to choose another location:

```sh
curl -fsSL https://coopr.strio.dev/install.sh | COOPR_INSTALL_PREFIX="$HOME/tools/coopr" sh
```

You can also download the [installer](../install.sh) or release archives directly. Corresponding dependency sources and rebuild instructions are in each release's `coopr-sources.tar.gz` archive.

## Requirements

- Linux with user and mount namespaces enabled; rootless builds need subordinate UID/GID ranges and working `newuidmap`/`newgidmap` helpers.
- A host configured for [rootless container builds](../guides/security.md#native-rootless-requirements).
- An OCI runtime such as `crun`, working UID-map helpers, and the networking helpers required by your rootless container configuration. The release binary does not install these host tools.
- Registry access for uncached image inputs. The first tutorial uses Red Hat UBI9 and installs NGINX from its public package repositories.

Other kernels, namespace policies, and nested-container environments can reject builds. See [security and rootless execution](../guides/security.md) for the container profile and limitations. Multi-platform assembly is supported; executing foreign binaries needs host `binfmt_misc` emulation.

## Build something useful

[Build your first image](../tutorials/first-image.md) serves a web page with NGINX. [Reuse two components](../tutorials/reusable-components.md) packages settings and policy files and applies them in order to that image.

A plain `--tag NAME` names a result locally. Publishing uses an explicit registry destination; see [builds](../guides/builds.md) and [storage](../guides/storage.md).

## Run the published container

The Coopr image is available from GHCR:

```sh
podman run --rm --network=none ghcr.io/strio-dev/coopr:latest --help
```

Use a release tag in place of `latest` to select an exact version. To build images inside the container, use the [nested rootless profile](../guides/security.md#nested-container-profile) with writable project and state mounts.

## Build from source with Nix

You can also build Coopr from a checkout using Nix with flakes enabled. From the repository root:

```sh
nix build path:.#coopr -o result-coopr
./result-coopr/bin/coopr --help
```

For the development shell, source-edit workflow, and tests, see [Contributing](../contributing.md).
