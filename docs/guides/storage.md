# Image and component storage

Coopr builds and consumes images directly in the effective `containers/storage` store used by Podman and Buildah. Podman does not need to be installed. Components use a separate OCI layout.

| Data | Typical rootless location |
| --- | --- |
| Images, instruction snapshots and cache mounts | `$XDG_DATA_HOME/containers/storage` |
| Component artifacts | `$XDG_DATA_HOME/coopr/components` |
| Native runtime state | `$XDG_RUNTIME_DIR/containers` |

Data home defaults to `~/.local/share`. Native `storage.conf` can override these paths, the driver and driver options. Rootful storage normally uses `/var/lib/containers/storage` and `/run/containers/storage`.

Ordinary output tags are visible to Podman and Buildah using compatible storage settings and the same user identity. Local FROM inputs use those native names; a permitted pull writes registry inputs into the same store. Coopr's activity lease does not exclude external Podman or Buildah processes.

The packaged container persists images, components and caches under `/var/lib`; this internal store does not automatically share the host’s Podman store. Use the volume in the [getting-started command](../getting-started/index.md#run-the-published-container).

## Copy explicitly

```sh
coopr copy app:dev local:app:alias
coopr copy app:dev oci-archive:app.oci.tar
coopr copy app:dev podman:app:dev
coopr copy app:dev docker:app:dev
coopr copy app:dev registry:registry.example.com/team/app:dev
```

Copies retain the local result. Import an engine image with the same command:

```sh
coopr copy podman:app:dev local:app:imported
```

`docker:SOURCE` also selects an engine source. Ordinary FROM inputs resolve from native storage or a registry.

Copy complete indexes to registries, OCI archives, Podman, or Docker’s containerd store. Docker’s classic store needs `--platform` to select one child. Docker imports may convert format and change the digest. Components support local names, OCI archives, and registries, but no engine destinations.

Appending with `coopr build --manifest NAME` updates an existing native manifest list in place, as Podman does. Mutable aliases of that list follow the update; previously returned manifest digests keep selecting their original index. An ordinary pulled image is not a native manifest list.

## Inspect and maintain

```sh
coopr image ls
coopr image inspect app:dev
coopr component ls
coopr system df
coopr system prune --dry-run
coopr cache prune --dry-run
```

Review the dry run, then omit `--dry-run` to prune. Component pruning protects named artifacts. Image pruning retains native image records because the shared store also belongs to other tools. Cache pruning removes Coopr instruction-cache aliases while retaining their image bytes. See [configuration](../reference/configuration.md) for native storage overrides.
