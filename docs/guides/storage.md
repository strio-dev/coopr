# Image and component storage

Coopr builds and consumes images directly in the effective `containers/storage` store used by Podman and Buildah. Podman does not need to be installed. Components use a separate OCI layout.

| Data | Typical rootless location |
| --- | --- |
| Images and instruction snapshots | `$XDG_DATA_HOME/containers/storage` |
| RUN cache mounts | `<effective graphroot>-tmp` |
| Component artifacts | `$XDG_DATA_HOME/coopr/components` |
| Native runtime state | `$XDG_RUNTIME_DIR/containers` |

Data home defaults to `~/.local/share`. Native `storage.conf` can override image and runtime storage paths, the driver and driver options. Component storage follows the data home independently. Rootful storage normally uses `/var/lib/containers/storage` and `/run/containers/storage`.

Ordinary output tags are visible to Podman and Buildah using compatible storage settings and the same user identity. Local FROM inputs use those native names; a permitted pull writes registry inputs into the same store. Coopr's activity lease does not exclude external Podman or Buildah processes.

Build and run directly with Podman:

```sh
coopr build image.coopr --tag app:dev
podman run --rm localhost/app:dev
```

Short output names receive the `localhost/` prefix. No copy or load step is needed when both tools use the same native store.

The packaged container persists images, components and caches under `/var/lib`; this internal store does not automatically share the host’s Podman store. Use the volume in the [getting-started command](../getting-started/index.md#run-the-published-container).

## Tags and transfers

Pull an image before a build, add a name, or publish it independently:

```sh
coopr pull docker.io/redhat/ubi9:latest
coopr tag app:dev registry.example.com/team/app:dev
coopr login registry.example.com
coopr push registry.example.com/team/app:dev
coopr logout registry.example.com
```

`login` uses the same credential file and helpers as Podman and Buildah. For automation, use `--password-stdin` with `--username`; see the [security guide](security.md). Standalone `pull` defaults to `--policy always`; builds still default to pulling missing image inputs.

Save and load images without rebuilding:

```sh
coopr save --output app.tar app:dev
coopr load --input app.tar
coopr save --format oci-archive --output app.oci.tar app:dev
coopr save --multi-image-archive --output images.tar app:dev docker.io/redhat/ubi9:latest
coopr save --format oci-dir --output app-layout app:dev
coopr load --input app-layout
```

Archives contain layers and image configuration. OCI saves include the selected image or all resident members of a stored index. Native export can change layer compression and the resulting manifest or index digest. Supply `--platform linux/amd64` when selecting one platform from an OCI index. Without `--multi-image-archive`, Docker-archive operands after the first image assign additional tags to that image. `load --input https://HOST/PATH/image.tar` downloads an archive before importing it.

```sh
coopr copy app:dev local:app:alias
coopr copy app:dev oci-archive:app.oci.tar
coopr copy app:dev docker:app:dev
coopr copy app:dev registry:registry.example.com/team/app:dev
```

Copies retain the local result. Docker images can be imported into native storage:

```sh
coopr copy docker:app:dev local:app:imported
```

Ordinary FROM inputs resolve from native storage or a registry.

Copy complete indexes to registries, OCI archives, or Docker’s containerd store. Podman reads locally stored indexes directly. Docker’s classic store needs `--platform` to select one child. Docker imports may convert format and change the digest. Components support local names, OCI archives, and registries, but no engine destinations.

Appending with `coopr build --manifest NAME` updates an existing native manifest list in place, as Podman does. Mutable aliases and the native list ID follow the update; prior index digests keep selecting their original index. An ordinary pulled image is not a native manifest list.

## Manage image manifest lists

Combine platform images already built locally:

```sh
coopr manifest create --annotation org.opencontainers.image.version=dev app:multi app:amd64
coopr manifest add --arch arm64 --annotation org.opencontainers.image.title=app app:multi app:arm64
coopr manifest inspect app:multi
coopr manifest inspect registry.example.com/team/app:dev
coopr manifest push app:multi registry.example.com/team/app:dev
```

`manifest add` can also read remote images; native manifest push fetches their contents when publishing the list. `--all` on create/add includes all members of an input index. Push includes all members by default. Create accepts `--annotation KEY=VALUE` for index annotations; add accepts instance annotations and platform overrides. `manifest annotate LIST IMAGE` selects a member by image name or digest; use `--index` without a member to annotate the list. These operations change metadata, without rebuilding image contents. `manifest push --rm` removes the local list after publication succeeds.

## Inspect and maintain

```sh
coopr images --filter reference='localhost/app:*' --sort repository
coopr images --format '{{.Repository}}:{{.Tag}}'
coopr image inspect app:dev docker.io/redhat/ubi9:latest
coopr image inspect --format '{{.ID}}' app:dev
coopr history app:dev
coopr component ls
coopr info
coopr system df --verbose
coopr system df --format json
coopr system prune --dry-run
coopr image prune --dry-run
```

Image inspection returns a JSON array of native image metadata and configuration. For a manifest list, it selects the host platform. Use `coopr manifest inspect NAME` for the list's child manifests and their platforms. Image lists and history use readable ages and sizes.

`image rm NAME` removes that tag and deletes the image when no other names retain it and no container uses it. Its output distinguishes untagged names from deleted image IDs. Internal cache references can keep an image resident after its public tag is removed. `--ignore` tolerates missing images, `--force` also permits removing dependent storage containers, `--all` selects all images, and `--no-prune` retains dangling parents. Removal returns 1 for missing-only failures, 2 for in-use images, and 125 for other errors, after reporting any successful removals. Like Buildah, forced removal operates on storage containers; it does not stop Podman processes or update Podman’s container database.

`system df` reports image counts, active images, readable sizes, and uniquely reclaimable image data. The cached-image row counts instruction snapshots; its size measures their filesystem layers, already included in the image total, and is unavailable when layer metadata is incomplete. Component active/reclaimable figures are shown as unavailable.

Omit `--dry-run` to prune. Both commands ask for confirmation; use `--force` (`-f`) in automation to skip the prompt. They remove dangling native images and unused instruction snapshots using the same image-pruning library as Podman. They retain user tags, images used by containers, and intermediate snapshots needed by other images or manifest lists. Deleting an image reclaims its layers when nothing else uses them. `system prune` also removes unnamed component content and retains named components.

Use `--all` to remove all unused images, including named images, and clear persistent RUN cache mounts. This also applies to images created by Podman or Buildah in the shared store. To restrict image pruning, use repeatable label or age filters:

```sh
coopr image prune --filter label=org.example.disposable=true --filter until=168h
```

Filters apply to image candidates, not component content or RUN cache mounts. To clear RUN mounts while keeping the default image-pruning rules:

```sh
coopr image prune --build-cache
```

RUN mount cleanup uses Buildah's cache-cleaning helper with the same temporary directory as Coopr's build workers. It clears Coopr's mounts for the selected graphroot; external Podman or Buildah builds may use a different temporary directory. Avoid pruning mounts while an external build uses them. Plain pruning leaves mount caches intact, and none of these commands delete portable caches configured with `--cache-from` or `--cache-to`.

`--dry-run` reports initial image candidates and component roots. It does not delete images, remove cache lookup names, or clear mounts. Actual pruning can also remove newly dangling parents. Coopr does not prune Podman's containers, pods, networks, or volumes. See [configuration](../reference/configuration.md) for native storage overrides.

Pruning reports removed names and images, followed by reclaimed space measured from native image usage and component blobs. The reclaimed-space figure excludes RUN cache mount directories. If damaged image metadata prevents measurement, pruning warns and reports the total as unavailable. Removal errors still cause the command to fail, with successful removals reported before the error.
