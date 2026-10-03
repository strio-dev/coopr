# Configuration

Coopr reads `$XDG_CONFIG_HOME/coopr/config.toml`, normally `~/.config/coopr/config.toml`. A missing file selects the private image store.

```toml
image-store = "coopr"
```

The only Coopr setting is `image-store`, accepting `coopr` or `podman` case-insensitively. Unknown keys, malformed TOML, an explicitly empty value, and unsupported store names fail. Global `--image-store` overrides a valid configuration value; configuration is still parsed first.

## Native storage

After choosing a store, these global flags override effective native storage settings:

| Flag | Purpose |
| --- | --- |
| `--root` | Native graph root. |
| `--runroot` | Runtime root. |
| `--storage-driver`, `--storage-opt` | Driver and repeated driver options. |
| `--imagestore` | Separate native image directory, rather than engine selection. |
| `--transient-store` | Transient container metadata in the runtime root. |

Podman mode loads effective `storage.conf`, including user/rootless settings. Coopr catalogs are scoped to the effective storage configuration. Components remain in the separate Coopr OCI layout. See [storage](../guides/storage.md).

## Runtime and trust

`--module`, `--cgroup-manager`, `--cdi-spec-dir`, `--network-config-dir`, and `--network-cmd-path` configure supervised execution. Native `containers.conf`, registries configuration, credentials, and signature policy still apply at their boundaries.

Registry requests accept `--authfile`, `--cert-dir`, `--creds`, `--tls-verify`, `--retry`, and `--retry-delay`; image inputs additionally support `--signature-policy` and `--decryption-key`. Credential helpers must be available in the execution environment.

`SOURCE_DATE_EPOCH` influences reproducible creation times. Explicit CLI values take precedence. `--timestamp` forces creation/new-layer file timestamps and conflicts with explicit source-date-epoch and rewrite-timestamp. `--cache-ttl` uses wall-clock publication age independently of those image times. Detailed rules live in the [execution contract](execution.md).
