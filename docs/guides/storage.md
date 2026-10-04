# Image and component storage

Choose where images are built and retained: Coopr’s private graph or Podman’s graph. Components use a separate OCI store in either mode.

| Data | Default location |
| --- | --- |
| Image graph and local selections | `$XDG_DATA_HOME/coopr/images` |
| Component artifacts | `$XDG_DATA_HOME/coopr/components` |
| Runtime state | `$XDG_RUNTIME_DIR/coopr/buildah` when available |

Data home defaults to `~/.local/share`. Instruction snapshots and cache mounts share the selected graph; deleting it also loses built and imported images.

## Select Podman storage

Set `image-store = "podman"` in `$XDG_CONFIG_HOME/coopr/config.toml`, or override per command:

```sh
coopr --image-store podman build image.coopr --tag app:dev
coopr --image-store coopr build image.coopr --tag private-app:dev
```

In Podman mode, ordinary output tags are visible to Podman. Coopr uses the effective native storage configuration and user mappings; both tools must use compatible settings when sharing a graph. Coopr’s activity lease does not exclude external Podman processes.

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

`docker:SOURCE` also selects an engine source. Ordinary FROM inputs resolve from the selected graph or a registry.

Copy complete indexes to registries, OCI archives, Podman, or Docker’s containerd store. Docker’s classic store needs `--platform` to select one child. Docker imports may convert format and change the digest. Components support local names, OCI archives, and registries, but no engine destinations.

## Inspect and maintain

```sh
coopr image ls
coopr image inspect app:dev
coopr component ls
coopr system df
coopr system prune --dry-run
coopr cache prune --dry-run
```

Review the dry run, then omit `--dry-run` to prune. Named and in-use roots are protected; unrelated objects in a shared Podman graph are retained. Pruning applies to the selected graph. See [configuration](../reference/configuration.md) for native storage overrides.
