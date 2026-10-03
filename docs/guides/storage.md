# Image and component storage

Coopr separates image execution/storage from component artifacts. Selecting Podman's graph affects images; components always retain their own OCI store.

| Data | Default location |
| --- | --- |
| Image graph and local selections | `$XDG_DATA_HOME/coopr/images` |
| Component artifacts | `$XDG_DATA_HOME/coopr/components` |
| Runtime state | `$XDG_RUNTIME_DIR/coopr/buildah` when available |

The data-home fallback is `~/.local/share`. Instruction snapshots and cache mounts share the selected image graph. Removing that graph loses locally built images and imported image inputs as well as caches.

## Select Podman storage

Set `image-store = "podman"` in `$XDG_CONFIG_HOME/coopr/config.toml`, or override per command:

```sh
coopr --image-store podman build image.coopr --tag app:dev
coopr --image-store coopr build image.coopr --tag private-app:dev
```

Podman mode uses effective native storage configuration and user mappings. Ordinary output names become visible to Podman. Catalogs below Coopr's data directory hold selection metadata, not another payload graph. Shared storage requires compatible configuration and native locking; Coopr's activity lease does not exclude external Podman processes.

## Copy explicitly

```sh
coopr copy app:dev local:app:alias
coopr copy app:dev oci-archive:app.oci.tar
coopr copy app:dev podman:app:dev
coopr copy app:dev docker:app:dev
coopr copy app:dev registry:registry.example.com/team/app:dev
```

These retain the local result. Engine sources `podman:SOURCE` and `docker:SOURCE` can also be imported through `coopr copy`. Ordinary FROM inputs resolve from the selected image graph or a registry.

A complete locally built index can be copied as a whole to a registry, OCI archive, Podman, or Docker's containerd store. Docker's classic store requires `--platform` to select one child. Single-image Docker imports can convert manifest format and change the digest. Components support local names, OCI archives, and registries, but cannot be copied into engine image stores.

## Inspect and maintain

```sh
coopr image ls
coopr image inspect app:dev
coopr component ls
coopr system df
coopr system prune --dry-run
coopr cache prune --dry-run
```

Review a dry run before pruning. Named and in-use roots are protected; unrelated objects in a selected shared Podman graph are retained. Docker and other unselected graphs are outside this scope. See [configuration](../reference/configuration.md) for low-level native storage overrides.
