# Getting started

Install Coopr on Linux (amd64 or arm64), then build an OCI container image using a reusable component. The tutorial uses Podman to run the image; see [host setup](#set-up-your-host) if you need it.

## Install

```sh
curl -fsSL https://coopr.strio.dev/install.sh | sh
coopr --version
```

This installs the latest stable release. For a specific version or Nix, see [other installation options](#other-installation-options).

## Run in a container

The published image includes Coopr and its build helpers. Use it with Podman to build images without installing Coopr on your host.

[Run Coopr in a container](../guides/container.md){ .md-button .coopr-container-link }

## Build your first image

[**Build hello world with a component**
<span>Create a component that packages a greeting, apply it to an image, and run the result.</span>
<span class="coopr-tutorial__action">Start tutorial</span>](../tutorials/first-image.md){ .coopr-path .coopr-tutorial }

## Set up your host

Install Podman using [your distribution's instructions](https://podman.io/docs/installation) and complete its [rootless setup](https://github.com/podman-container-tools/podman/blob/main/docs/tutorials/rootless_tutorial.md). Run this basic check as your normal user:

```sh
podman run --rm docker.io/redhat/ubi9:latest true
```

The container prints no output and exits successfully. Run Coopr and Podman as the same user with the same storage settings so the tutorial can build and run images without a transfer step. Podman is needed to run the tutorial's image; Coopr itself does not require it.

The Coopr binary embeds the Buildah backend, but does not install host tools. Native rootless builds need:

- User and mount namespaces, subordinate UID/GID ranges, and working `newuidmap` and `newgidmap` helpers.
- A supported container storage configuration and an OCI runtime such as `crun`. Overlay storage may need `fuse-overlayfs`.
- The helpers required by your network configuration for networked build steps, and registry access for uncached base images.

The Podman check does not verify every Coopr build prerequisite. See [security and rootless builds](../guides/security.md#native-rootless-requirements) for isolation, resource limits, and nested builds.

## Other installation options

### Install a specific version

Pass a published release tag to the installer:

```sh
curl -fsSL https://coopr.strio.dev/install.sh | sh -s -- 0.2.0
```

You can also download binary archives from [GitHub releases](https://github.com/strio-dev/coopr/releases). Extract the complete archive and keep its accompanying `libexec/` directory beside `coopr`; RUN blocks with multiple commands require its helper. The installer handles this layout automatically.

Each release also includes [corresponding sources and rebuild instructions](../contributing.md#corresponding-source).

### Install with Nix

With Nix flakes enabled:

```sh
nix profile add github:strio-dev/coopr
coopr --version
```

This uses the repository's default branch. See [contributing](../contributing.md) to build from a checkout or set up a development shell.
