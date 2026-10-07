# Security and rootless builds

Build definitions and components execute code. Review their inputs, transformations, and requested privileges before running them. A verified digest establishes byte identity; it does not establish publisher trust.

## Native rootless requirements

For native rootless builds, provide:

- Linux user and mount namespaces, subordinate UID/GID mappings, and a supported storage driver.
- An OCI runtime and, for networked RUNs, the configured network stack and helper.
- Delegated cgroup controllers when using memory or CPU limits. Rootless cgroup v1 or missing delegation fails explicitly.

Authorize host-network RUNs explicitly:

```sh
coopr build image.coopr --allow network.host
```

`run network="host"` requires `network.host`, including on cache hits. Insecure RUNs require `--allow security.insecure` and OCI/rootless isolation; chroot is rejected. CDI devices require `device` or a matching `device=SELECTOR` entitlement unless authorized by CDI metadata. These authorizations cannot grant privileges unavailable in the outer environment.

## Credentials and remote sources

Supply secret and SSH sources per build:

```sh
coopr build image.coopr --secret id=token,src=./token --ssh default
```

Keep credentials out of the context and image layers. Use `--no-cache` when changed credentials must change a cached result; credential bytes and SSH agent contents do not invalidate it. See [caching](caching.md#refresh-deliberately).

HTTPS Git accepts host-scoped `GIT_AUTH_HEADER.<host>` or `GIT_AUTH_TOKEN.<host>` secrets. Authenticated Git rejects redirects and withholds parent credentials from out-of-scope submodules. SSH Git needs `GIT_KNOWN_HOSTS[.host]` and an SSH source. HTTP ADD selects authorization per redirect host. Registry requests use the configured authentication, certificate, TLS, and retry options.

Image inputs honor native registry routing and signature policy. Keep TLS verification enabled outside deliberately configured local test registries. For immutable selection, pin image and component references by digest, including nested references. A component digest alone does not establish publisher trust or signature policy.

## Nested container profile

The scratch-based container image includes Coopr's embedded Buildah backend, `fuse-overlayfs`, `crun`, network and UID-map helpers, Git/SSH, GPGME/GnuPG, certificates, and archive support. It runs as UID/GID 0 inside the container; with rootless Podman, that identity maps to the invoking host user. It has no distribution package manager or builder daemon.

Use the [getting-started command](../getting-started/index.md#run-the-published-container) to expose `/dev/fuse`, permit the required mounts with unconfined seccomp and disabled labeling, mount a writable project, and persist `/var/lib`. The image configures `fuse-overlayfs` for its overlay store and keeps images, components, and caches in that volume.

Volumes mounted at the previous UID/GID 1000 image's `/home/user/.local/share` path are no longer used; preserve or back up their state before replacing them.

The packaged default is `BUILDAH_ISOLATION=chroot`. RUNs share the outer container's network, IPC, PID, and cgroup namespaces. Apply offline restrictions with outer Podman `--network=none`, and resource limits with outer Podman memory and CPU options. Per-RUN network isolation and cgroup memory or CPU controls require OCI isolation.

Select OCI explicitly with `coopr build --isolation=rootless` or `--isolation=oci` (after the image name when using Podman). Nested OCI execution also needs user/mount namespaces, working UID-map helpers, and outer policies permitting clone/unshare/mount; network helpers may need `/dev/net/tun`. Insecure RUNs still require their entitlement and OCI/rootless isolation. OCI failures are reported without an automatic chroot fallback.

## Supported limitations

- Only Linux target platforms are supported. Foreign RUNs need host `binfmt_misc` emulation; Coopr neither registers handlers nor bundles QEMU.
- Root-directory mode, ownership, and portable xattrs can be lost by the upstream image commit path even when Coopr captures them in package/cache state. The build can succeed with this limitation.
- Healthcheck and ONBUILD extensions are preserved with OCI/Docker output; receiving runtimes decide whether to honor them.
- Docker's classic store requires a selected child for multi-platform transfer. Auxiliary attestation descriptors and duplicate runnable platform entries are rejected by current image selection.
- Keyless signing is unsupported. Keyed Sigstore signatures do not include a Rekor transparency-log entry.
- Mutable external inputs may not invalidate caches; see [refresh rules](caching.md#refresh-deliberately).
- The tested host profile does not establish support for every kernel or CI runtime policy.
