# Configuration

Coopr reads `$XDG_CONFIG_HOME/coopr/config.toml`, normally `~/.config/coopr/config.toml`. A missing file selects the private image store.

```toml
image-store = "coopr"
```

`image-store` is the only setting. It accepts `coopr` or `podman`, case-insensitively. Unknown keys, malformed TOML, empty values, and unsupported stores fail. Global `--image-store` overrides a valid value; configuration is parsed first.

## Native storage

These global flags override the selected store's effective native settings:

| Flag | Purpose |
| --- | --- |
| `--root` | Native graph root. |
| `--runroot` | Runtime root. |
| `--storage-driver`, `--storage-opt` | Driver and repeatable options. |
| `--imagestore` | Separate native image directory; distinct from `--image-store`. |
| `--transient-store` | Transient container metadata in the runtime root. |

Podman mode loads effective `storage.conf`, including rootless/user settings. Coopr catalogs follow the effective storage configuration; components stay in their separate OCI layout. See [storage](../guides/storage.md).

## Runtime and trust

| Controls | Purpose |
| --- | --- |
| `--module`, `--cgroup-manager`, `--cdi-spec-dir`, `--network-config-dir`, `--network-cmd-path` | Configure supervised execution. |
| `--authfile`, `--cert-dir`, `--creds`, `--tls-verify`, `--retry`, `--retry-delay` | Registry authentication, trust, and retries. |
| `--signature-policy`, `--decryption-key` | Image-input signature policy and decryption. |

Native `containers.conf`, registry settings, credentials, and signature policy apply at their respective boundaries. Credential helpers must be available in the execution environment.

Ambient `SOURCE_DATE_EPOCH` sets creation time unless overridden explicitly. `--timestamp` forces creation/new-layer file times and conflicts with source-date-epoch and rewrite-timestamp. `--cache-ttl` uses wall-clock publication age independently of image times. See [timestamp rules](execution.md#timestamps-and-cache-age).
