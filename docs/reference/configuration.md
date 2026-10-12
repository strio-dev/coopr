# Configuration

Coopr uses native container configuration for image storage, registry access and RUN execution. Global options select host settings; build and transfer options apply to that command. Use `coopr COMMAND --help` for its available options.

## Configuration files

| File | Controls |
| --- | --- |
| [storage.conf](https://github.com/podman-container-tools/container-libs/blob/6eac452fac5c9d9ad8f86b6fd6109f8f32659f77/storage/docs/containers-storage.conf.5.md) | Image storage paths, driver and driver options. |
| [containers.conf](https://github.com/podman-container-tools/container-libs/blob/3bbb182d1fbf4c2f1566446701421174cbcff580/common/docs/containers.conf.5.md) | Supported runtime, network, helper and image-compression defaults. |
| [registries.conf](https://github.com/podman-container-tools/container-libs/blob/b5becb3ba69affb5026fbbefe41cb4199a186482/image/docs/containers-registries.conf.5.md) | Registry routing, mirrors, blocked registries, credential helpers and insecure endpoints. |
| [policy.json](https://github.com/podman-container-tools/container-libs/blob/b5becb3ba69affb5026fbbefe41cb4199a186482/image/docs/containers-policy.json.5.md) | Image acceptance and signature requirements. |
| [auth.json](https://github.com/podman-container-tools/container-libs/blob/b5becb3ba69affb5026fbbefe41cb4199a186482/image/docs/containers-auth.json.5.md) | Registry credentials; managed with `coopr login` and `coopr logout`. |

The native loaders select system/user files and supported drop-ins. Each file has its own precedence rules; the linked manuals match the libraries used by Coopr. Explicit command options override the settings they control. Container configuration includes settings for other engines, so not every field applies to Coopr.

## Native storage

These global options override the effective native settings:

| Option | Purpose |
| --- | --- |
| `--root` | Persistent native graph root. |
| `--runroot` | Temporary native runtime root. |
| `--storage-driver`, `--storage-opt` | Driver and repeatable options. |
| `--imagestore` | Separate native image directory. |
| `--transient-store` | Transient container metadata in the runtime root. |

Coopr loads effective `storage.conf`, including rootless/user settings. Images built by Coopr are available to Podman and Buildah using the same user identity and compatible storage settings. Inspect the effective image store and host information with:

```sh
coopr info --format json
```

Component artifacts use a separate OCI layout at `$XDG_DATA_HOME/coopr/components`, or `~/.local/share/coopr/components` when unset. A nonempty `XDG_DATA_HOME` must be absolute. `--root` does not relocate components. See [storage locations](../guides/storage.md#where-data-lives) for image, component and cache paths.

## Runtime and trust

| Global option | Purpose |
| --- | --- |
| `--module NAME` | Load a native containers configuration module; repeatable. |
| `--cgroup-manager` | Select `systemd` or `cgroupfs` for RUN execution. |
| `--cdi-spec-dir DIR` | Select CDI specification directories; repeatable. |
| `--network-config-dir DIR` | Select the native network configuration directory. |

Build-specific settings such as `--runtime`, `--isolation` and `--network` belong to the build command. See [RUN and lifecycle controls](execution.md#build-request-controls) for their scope and defaults. Native helper executables use `PATH` and `containers.conf`'s `[engine] helper_binaries_dir` setting; credential helpers must also be available in the execution environment.

### Registry access

Registry options are command-local. Check that command's help for the options it accepts:

| Option | Purpose |
| --- | --- |
| `--authfile PATH` | Select a containers-auth.json or Docker config.json credential file. |
| `--creds USER[:PASSWORD]` | Supply credentials for the request, overriding the auth file. |
| `--cert-dir DIR` | Select registry CA and optional client certificates. |
| `--tls-verify` | Override the registry's TLS/HTTP policy as described below. |
| `--retry N` | Override retries after the first attempt (normally 3). |
| `--retry-delay DURATION` | Set a fixed delay; otherwise use the effective native retry policy. |

Builds and image/component transfers inherit `[engine] retry` and `retry_delay` from `containers.conf` when `--retry` is omitted. Manifest-member additions default to three retries.

On Linux, `coopr login` normally writes `$XDG_RUNTIME_DIR/containers/auth.json`, which does not survive reboot. Default credential lookup can also read user configuration and Docker credential files or use helpers. An explicit `--authfile` selects that file instead of the usual fallback search. Use the same file on later build or transfer commands; see [registry login](../guides/security.md#log-in-to-a-registry).

Image, component and registry cache transfers use Podman/Buildah's registry transport. Registry routing, mirrors, authentication and certificates follow native configuration. TLS behavior is:

| `--tls-verify` | Behavior |
| --- | --- |
| Omitted | Honor `registries.conf`, including registries marked insecure. |
| `true` | Require verified HTTPS. |
| `false` | Try HTTPS without certificate verification and permit HTTP fallback. |

### Image trust

TLS controls connection security; `policy.json` controls image acceptance. The command-local `--signature-policy PATH` overrides image policy on both build commands, image pull/push/save/load, and manifest push. This option is hidden from help and does not verify component signatures.

`--decryption-key` decrypts image inputs on both build commands and image pull. It is separate from signing and TLS. See [image and component trust](../guides/security.md#trust-images-and-components).

## Environment defaults

| Variable | Effect |
| --- | --- |
| `BUILDAH_FORMAT` | Default image format when `--format` is omitted; otherwise `oci`. |
| `BUILDAH_LAYERS` | `false` (case-insensitive) or `0` disables instruction layers by default; other values leave them enabled. Explicit `--layers` wins. |
| `BUILDAH_ISOLATION` | Native RUN isolation default; `--isolation` overrides it. The published container sets `chroot`. |
| `SOURCE_DATE_EPOCH` | Nonnegative Unix epoch seconds for creation-time policy. `--source-date-epoch` overrides it; an empty environment value is invalid. |
| Host proxy variables | Passed to RUN by default; `--http-proxy=false` disables host forwarding. See [proxy arguments](execution.md#arguments-and-normalization) for explicit build arguments. |

`--timestamp` forces creation/new-layer file times and conflicts with an epoch supplied through `SOURCE_DATE_EPOCH` or `--source-date-epoch`, and with `--rewrite-timestamp`. `--cache-ttl` uses wall-clock publication age independently of image times. See [timestamp rules](execution.md#timestamps-and-cache-age) for authored epoch sources and cache behavior.
