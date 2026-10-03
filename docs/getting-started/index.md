# Getting started

Coopr builds Linux images and reusable OCI components. Build from a checkout with Nix, then run the native CLI on a Linux host.

## Requirements

- Linux with user and mount namespaces enabled; rootless builds need subordinate UID/GID ranges and working `newuidmap`/`newgidmap` helpers.
- A host configured for [rootless container builds](../guides/security.md#native-rootless-requirements).
- Nix with flakes enabled for the pinned build and development environment.
- Registry access for uncached image inputs. The first tutorial uses Red Hat UBI9 and installs NGINX from its public package repositories.

Other kernels, namespace policies, and nested-container environments can reject builds. See [security and rootless execution](../guides/security.md) for the container profile and limitations. Multi-platform assembly is supported; executing foreign binaries needs host `binfmt_misc` emulation.

## Build the CLI

From the repository root:

```sh
nix build path:.#coopr -o result-coopr
./result-coopr/bin/coopr --help
nix develop path:.
```

Inside the development shell, `coopr` is ready to use:

```sh
coopr --help
```

The shell provides the packaged CLI built from your checkout. To run current source edits without rebuilding that package, use:

```sh
go run ./cmd/coopr --help
```

Direnv loads the same environment.

Use `path:.` during development so Nix sees untracked source files. Dependencies and tools come from `go.sum` and `flake.lock`.

## Build something useful

[Build your first image](../tutorials/first-image.md) serves a web page with NGINX. [Reuse two components](../tutorials/reusable-components.md) packages settings and policy files and applies them in order to that image.

A plain `--tag NAME` names a result locally. Publishing uses an explicit registry destination; see [builds](../guides/builds.md) and [storage](../guides/storage.md).

## Build the CLI container

```sh
nix build path:.#container.copyTo -o result-container-copy
podman_store=$(podman info --format '{{.Store.GraphDriverName}}@{{.Store.GraphRoot}}+{{.Store.RunRoot}}')
podman unshare ./result-container-copy/bin/copy-to "containers-storage:[$podman_store]localhost/coopr:nix"
podman run --rm --network=none localhost/coopr:nix --help
```

Running `--help` does not exercise nested builds. Actual builds need the [nested rootless profile](../guides/security.md#nested-container-profile), writable project/state mounts, and the required outer permissions.
