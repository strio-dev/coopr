# Configuration

Coopr uses native `containers/storage` configuration for images and keeps component artifacts in a separate OCI layout.

## Native storage

These global options override the effective native settings:

| Option | Purpose |
| --- | --- |
| `--root` | Native graph root. |
| `--runroot` | Runtime root. |
| `--storage-driver`, `--storage-opt` | Driver and repeatable options. |
| `--imagestore` | Separate native image directory. |
| `--transient-store` | Transient container metadata in the runtime root. |

Coopr loads effective `storage.conf`, including rootless/user settings. Images built by Coopr are available to Podman and Buildah using the same configuration and user identity. Components stay in their separate OCI layout. See [storage](../guides/storage.md).

## Runtime and trust

| Controls | Purpose |
| --- | --- |
| `--module`, `--cgroup-manager`, `--cdi-spec-dir`, `--network-config-dir` | Configure supervised execution. |
| `--authfile`, `--cert-dir`, `--creds`, `--tls-verify`, `--retry`, `--retry-delay` | Registry authentication, trust, and retries. |
| `--decryption-key` | Image-input decryption on both build commands. |

Native `containers.conf`, registry settings, credentials, and signature policy apply at their respective boundaries. Credential helpers must be available in the execution environment.

Image, component, and registry cache transfers use Podman/Buildah's registry transport. Without an explicit `--tls-verify`, `registries.conf` controls TLS and HTTP access. `--tls-verify=true` requires verified HTTPS; `--tls-verify=false` tries HTTPS with certificate verification disabled and permits HTTP fallback. Registry routing, mirrors, blocked registries, authentication, and certificates follow the native configuration.

Native helper executables use `PATH` and `containers.conf`'s `[engine] helper_binaries_dir` setting.

Ambient `SOURCE_DATE_EPOCH` sets creation time unless overridden explicitly. `--timestamp` forces creation/new-layer file times and conflicts with source-date-epoch and rewrite-timestamp. `--cache-ttl` uses wall-clock publication age independently of image times. See [timestamp rules](execution.md#timestamps-and-cache-age).
