# Run Coopr in a container

Run builds with the published Coopr image, which includes the CLI and its build helpers. Use a project bind mount for source files and a named volume to keep build state between runs.

## Before you start

You need Linux, working rootless Podman, and access to `/dev/fuse`. Your host must permit the [nested container profile](security.md#nested-container-profile) used below. The first run pulls Coopr from GHCR; the example build also pulls UBI9 from Docker Hub.

Check the CLI:

```sh
podman run --rm ghcr.io/strio-dev/coopr:latest --help
```

Replace `latest` with the same published release tag in each command to select a specific Coopr version.

## Build an image

Create `components/greeting.coopr` and `image.coopr` from [Build hello world](../tutorials/first-image.md#write-the-component). From the directory containing `image.coopr`, build with:

```sh
podman run --rm \
  --device=/dev/fuse:rw \
  --security-opt=seccomp=unconfined \
  --security-opt=label=disable \
  -v "$PWD:/work:rw" \
  -v coopr-state:/var/lib \
  ghcr.io/strio-dev/coopr:latest \
  build --file /work/image.coopr --tag hello:latest /work
```

The mounts serve different purposes:

- `$PWD:/work:rw` makes the current project available as the build context and lets Coopr write output files back to it.
- `coopr-state:/var/lib` retains images, components, and caches after the container exits. Keep this volume mounted in subsequent commands to reuse them.

The image configures `fuse-overlayfs` and chroot isolation. The device option exposes FUSE; the security options disable seccomp filtering and SELinux labeling for this container. With rootless Podman, root inside the container maps to your host user. See the [nested container profile](security.md#nested-container-profile) for isolation and resource controls.

## Save the image

The build's image store lives in `coopr-state`, separate from the host's Podman store. Use the same volume to save the image to `hello.tar` in your project directory:

```sh
podman run --rm \
  --device=/dev/fuse:rw \
  --security-opt=seccomp=unconfined \
  --security-opt=label=disable \
  -v "$PWD:/work:rw" \
  -v coopr-state:/var/lib \
  ghcr.io/strio-dev/coopr:latest \
  save --output /work/hello.tar localhost/hello:latest
```

`save` writes a Docker archive by default, including the image's configuration, layers, and tag.

## Load and run with Podman

Load the archive into the host's store, then run the image:

```sh
podman load --input hello.tar
podman run --rm localhost/hello:latest
```

```{ .text .no-copy }
Hello, world!
```

The loaded image now lives in the host's Podman store. The separate `coopr-state` volume remains available for future builds. See [image storage and transfers](storage.md) for publishing to a registry or exporting other formats.
