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
```

Archives contain layers and image configuration. OCI saves retain the selected manifest or complete stored index. Supply `--platform linux/amd64` when selecting one platform from an OCI index.

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

Appending with `coopr build --manifest NAME` updates an existing native manifest list in place, as Podman does. Mutable aliases of that list follow the update; previously returned manifest digests keep selecting their original index. An ordinary pulled image is not a native manifest list.

## Manage image manifest lists

Combine platform images already built locally:

```sh
coopr manifest create app:multi app:amd64 app:arm64
coopr manifest inspect app:multi
coopr manifest push app:multi registry.example.com/team/app:dev
```

`manifest add` can also read remote images; native manifest push fetches their contents when publishing the list. `--all` on create/add includes all members of an input index. Push includes all members by default. Annotations describe the index or its members; they do not rebuild image contents.

## Inspect and maintain

```sh
coopr image ls
coopr image inspect app:dev
coopr history app:dev
coopr component ls
coopr info
coopr system df
coopr system prune --dry-run
coopr image prune --dry-run
```

Image inspection shows native image metadata and configuration. For a manifest list, it shows the child manifests and their platforms.

Omit `--dry-run` to prune. Both prune commands remove dangling native images and unused instruction snapshots using the same image-pruning library as Podman. They retain user tags, images used by containers, and intermediate snapshots needed by other images or manifest lists. Deleting an image reclaims its layers when nothing else uses them. `system prune` also removes unnamed component content and retains named components.

Use `--all` to remove all unused images, including named images, and clear persistent RUN cache mounts. This also applies to images created by Podman or Buildah in the shared store. To clear RUN mounts while keeping the default image-pruning rules:

```sh
coopr image prune --build-cache
```

RUN mount cleanup uses Buildah's cache-cleaning helper with the same temporary directory as Coopr's build workers. It clears Coopr's mounts for the selected graphroot; external Podman or Buildah builds may use a different temporary directory. Avoid pruning mounts while an external build uses them. Plain pruning leaves mount caches intact, and none of these commands delete portable caches configured with `--cache-from` or `--cache-to`.

`--dry-run` reports initial image candidates and component roots. It does not delete images, remove cache lookup names, or clear mounts. Actual pruning can also remove newly dangling parents. Coopr does not prune Podman's containers, pods, networks, or volumes. See [configuration](../reference/configuration.md) for native storage overrides.
