# Image and component storage

Coopr builds and consumes images in the same native store as Podman and Buildah. Components have a separate OCI artifact store. You can name, inspect, archive, and publish both without rebuilding them.

## Use images with Podman

From a repository checkout, build the [hello-world example](../tutorials/first-image.md) and run it with Podman:

```sh
coopr build examples/first-image/image.coopr --tag app:dev
podman run --rm localhost/app:dev
```

```{ .text .no-copy }
Hello, world!
```

Short output names receive the `localhost/` prefix. When both tools run as the same user with compatible storage settings, no copy or load step is needed. Coopr itself does not require Podman; Podman runs the resulting image here.

FROM checks native storage first and pulls missing registry inputs into that store by default. Explicit image transports are also supported; see [image selection](../reference/execution.md#image-selection).

When Coopr runs in its published container, its internal store is separate from the host’s Podman store. Follow the [container guide](container.md) to persist it and transfer an image to the host.

## Tags and transfers

Add a registry name to the image built above, then publish it. Replace `registry.example.com/team/app:dev` with a reference you can write to:

```sh
coopr tag app:dev registry.example.com/team/app:dev
coopr login registry.example.com
coopr push registry.example.com/team/app:dev
coopr logout registry.example.com
```

Tagging adds a name to the existing local image. `copy` is an alias for `tag`; both accept ordinary names and print no success line. Push transfers the image and retains the local result.

`login` uses the same credential file and helpers as Podman and Buildah. For automation, use `--password-stdin` with `--username`; see the [security guide](security.md).

Pull a registry image into the local store before a build:

```sh
coopr pull docker.io/redhat/ubi9:latest
```

Standalone `pull` refreshes the image by default (`--policy always`). Builds default to pulling only missing inputs.

### Transfer to Docker Engine

With Docker Engine running, transfer the local image to it:

```sh
coopr push app:dev docker-daemon:app:dev
```

Import an Engine image into native storage with the same transport:

```sh
coopr pull docker-daemon:app:dev
coopr tag app:dev app:imported
```

`coopr pull docker-daemon:NAME` imports the single compatibility image exported by Docker. `--platform` validates that image's configuration; it does not select a child of a multi-platform Docker image. A mismatch fails instead of returning an image for the wrong platform. Registry pulls select the requested index platform.

To import a specific Docker platform, [Docker's platform-scoped save](https://docs.docker.com/reference/cli/docker/image/save/#platform) requires API 1.48 or later:

```sh
docker image save --platform=linux/arm64 --output app-arm64.tar app:dev
coopr pull --platform=linux/arm64 docker-archive:app-arm64.tar
```

Transport prefixes select transfer sources or destinations, not local tags. Other destinations include registries, image directories, archives, and `containers-storage:`; see [transfer destinations](../reference/cli.md#names-and-transfer-destinations).

## Save and load images

Save the image to a Docker archive, then load it on another machine:

```sh
coopr save --output app.tar app:dev
coopr load --input app.tar
```

Archives contain the image’s layers and configuration. Choose an OCI archive or layout directory explicitly:

```sh
coopr save --format oci-archive --output app.oci.tar app:dev
coopr save --format oci-dir --output app-layout app:dev
coopr load --input app-layout
```

An OCI save includes one image or a complete stored index. A partially stored index exports one selected image, rather than all resident members; use `--platform linux/amd64` to choose explicitly. Native export can change layer compression and manifest/index digests. Docker’s classic Engine store also needs one selected platform for an index; see [transfer contracts](../reference/execution.md#layers-storage-and-copying).

To save several images in one Docker archive, use `--multi-image-archive`:

```sh
coopr save --multi-image-archive --output images.tar app:dev docker.io/redhat/ubi9:latest
```

Without this option, extra operands assign tags to the first image. `load --input` also accepts an HTTP(S) archive URL. See [archive contracts](../reference/cli.md#archives-and-directories) for formats and index filtering.

## Store and share components

After [packaging a component](components.md#build-an-oci-component) as `shared`, inspect it, add another local name, or save it for another machine:

```sh
coopr component ls
coopr component inspect shared
coopr component tag shared shared:backup
coopr component save --output shared.oci.tar shared
coopr component load --input shared.oci.tar
```

Component archives retain all packaged platforms and files. Loading restores the archived name; `--tag NAME` chooses another. Components are build inputs, not runnable images, and do not appear in Podman’s image list.

Publish and pull through a registry you can access, using the same `coopr login` credentials as image transfers:

```sh
coopr component push shared registry.example.com/team/shared:1
coopr component pull registry.example.com/team/shared:1 --tag shared
```

Invocation checks the component store first, then resolves a missing name from a registry. A bare `sha256:` digest selects locally stored content; omit `--tag` from pull to print that digest. The [components guide](components.md) covers local definition paths, argument binding, and platform selection.

To remove just the extra local name:

```sh
coopr component rm shared:backup
```

This removes the name, not the artifact by digest. `system prune` can reclaim unnamed component content.

## Manage image manifest lists

Combine images already built locally as `app:amd64` and `app:arm64`:

```sh
coopr manifest create --annotation org.opencontainers.image.version=dev app:multi app:amd64
coopr manifest add --arch arm64 --annotation org.opencontainers.image.title=app app:multi app:arm64
coopr manifest inspect app:multi
coopr manifest inspect registry.example.com/team/app:dev
coopr manifest push app:multi registry.example.com/team/app:dev
```

Podman reads local indexes directly. `manifest add` can also read remote images and explicit image transports; push fetches their contents when publishing the list. Create/add `--all` includes every member of an input index, and push includes all members by default.

You can also build into a manifest list with `coopr build --manifest NAME`. This updates an existing native list in place: aliases and the native list ID follow the update, while prior index digests still select their original index. An ordinary pulled image is not a native manifest list.

Annotate members or the index without rebuilding their files. `manifest push --rm` removes the local list only after successful publication. See [manifest list options](../reference/cli.md#manifest-lists) for annotations, platform overrides, and removal.

## Inspect and maintain

```sh
coopr images --filter reference='localhost/app:*' --sort repository
coopr images --format '{{.Repository}}:{{.Tag}}'
coopr image inspect app:dev docker.io/redhat/ubi9:latest
coopr image inspect --format '{{.ID}}' app:dev
coopr history app:dev
coopr info
coopr system df --verbose
coopr system df --format json
```

Inspection returns a JSON array and selects the host platform for a manifest list. Use `coopr manifest inspect NAME` to see all child descriptors. For report filters and templates, see [image reports](../reference/cli.md#image-reports).

`image rm NAME` removes a tag and deletes the image when no names or containers retain it. Internal cache references can keep it resident. Use `--ignore` for missing images or `--no-prune` to keep dangling parents. `--force` permits removing dependent storage containers, but does not stop Podman processes or update Podman’s container database. See [transfer output and errors](../reference/cli.md#transfer-output-and-errors) for removal reporting and exit status.

`system df` reports image counts, active images, readable sizes, and uniquely reclaimable image data. The cached-image row counts instruction snapshots; its size measures their filesystem layers, already included in the image total, and is unavailable when layer metadata is incomplete. Component active/reclaimable figures are shown as unavailable.

### Prune unused data

Preview what would be removed:

```sh
coopr image prune --dry-run
coopr system prune --dry-run
```

Omit `--dry-run` to prune. Both commands ask for confirmation; `--force` (`-f`) skips the prompt. They remove dangling images and unused instruction snapshots, retaining named images, images used by containers, and intermediate layers needed by other images or manifest lists. `system prune` also removes unnamed component content.

Use `--all` to remove all unused images, including named images, and clear persistent RUN cache mounts. This also applies to images created by Podman or Buildah in the shared store. To restrict image pruning, use repeatable label or age filters:

```sh
coopr image prune --filter label=org.example.disposable=true --filter until=168h
```

Filters apply to image candidates, not component content or RUN cache mounts.

### Clear RUN cache mounts

To clear mount caches while keeping the default image-pruning rules:

```sh
coopr image prune --build-cache
```

This clears Coopr’s mounts for the selected graph root. External Podman or Buildah builds may use a different cache directory; avoid pruning mounts while an external build uses them. Plain pruning leaves mount caches intact. These commands do not delete portable caches configured with `--cache-from` or `--cache-to`.

`--dry-run` reports initial image candidates and component roots. It does not delete images, remove cache lookup names, or clear mounts. Actual pruning can also remove newly dangling parents. Coopr does not prune Podman's containers, pods, networks, or volumes. See [configuration](../reference/configuration.md) for native storage overrides.

Pruning reports removed names and images, followed by reclaimed space from native image usage and component blobs, excluding RUN cache mount directories. If image metadata prevents measurement, the total is reported as unavailable. Removal errors cause the command to fail after reporting successful removals.

## Where data lives

| Data | Typical rootless location |
| --- | --- |
| Images and instruction snapshots | `$XDG_DATA_HOME/containers/storage` |
| RUN cache mounts | `<graphroot>-tmp` |
| Component artifacts | `$XDG_DATA_HOME/coopr/components` |
| Native runtime state | `$XDG_RUNTIME_DIR/containers` |

`XDG_DATA_HOME` defaults to `~/.local/share`. The graph root is the image store directory selected by native `storage.conf`, which also controls runtime paths and the storage driver. Component storage follows the data home independently. Rootful native storage normally uses `/var/lib/containers/storage` and `/run/containers/storage`.

Coopr honors explicit `graphroot` and `runroot` settings even when running rootless. Native defaults apply when the effective configuration omits these keys or leaves them empty. To override system paths for one user, set these keys in `$XDG_CONFIG_HOME/containers/storage.conf` (normally `~/.config/containers/storage.conf`); see the [native storage configuration reference](https://github.com/podman-container-tools/container-libs/blob/6eac452fac5c9d9ad8f86b6fd6109f8f32659f77/storage/docs/containers-storage.conf.5.md).

The published Coopr container uses these native image-store defaults. See [configuration](../reference/configuration.md) for overrides and the [container guide](container.md) for persistent storage.
