# Security and rootless builds

Build definitions execute code. Review image inputs, component transformations, remote sources, and requested privileges before using untrusted artifacts. Descriptor verification establishes byte identity; it does not establish publisher trust.

## Native rootless requirements

Linux must permit user and mount namespaces, suitable subordinate UID/GID mappings, and a supported native storage driver. RUN uses the configured OCI runtime. Networked rootless RUNs need the configured network stack and helper. Memory/CPU controls require delegated cgroup controllers; rootless cgroup v1 or missing delegation fail explicitly.

Host network, insecure RUN, and devices require explicit authorization:

```sh
coopr build image.coopr --allow network.host
```

`run network="host"` needs that entitlement, including on cache hits. Insecure execution requires `--allow security.insecure`; CDI devices need `device` or a matching `device=SELECTOR` entitlement unless authorized by CDI metadata. Effective privileges remain bounded by the outer environment. Insecure execution requires OCI/rootless isolation; chroot is rejected.

## Credentials and remote sources

Supply secret and SSH sources per build:

```sh
coopr build image.coopr --secret id=token,src=./token --ssh default
```

Mount declarations affect cache identity; credential bytes and agent contents do not. Refresh cached results when changed credentials must change the output. Avoid copying credentials into the context or writing them into layers.

HTTPS Git uses host-scoped `GIT_AUTH_HEADER.<host>` or `GIT_AUTH_TOKEN.<host>` secrets. Authenticated Git rejects redirects and does not send a parent repository's authorization to out-of-scope submodules. SSH Git requires an explicit `GIT_KNOWN_HOSTS[.host]` secret and supplied SSH source. HTTP file ADD selects authorization per redirect host. Registry operations share request-scoped authentication, certificate, TLS, and retry options.

Image inputs honor native registry routing and signature policy. Pin every selected reference, including nested component/image inputs, when immutable selection matters. Component artifact digests do not establish a publisher signature policy by themselves. Keep TLS verification enabled outside deliberately configured local test registries.

## Nested container profile

The Nix scratch distribution includes Coopr, `crun`, networking helpers, UID-map helpers, Git/SSH, GPGME/GnuPG, certificates, and archive support. It runs as UID/GID 1000 and has no distribution package manager or builder daemon.

The Linux/amd64 acceptance profile needs nested user/mount namespaces, working setuid UID-map helpers, namespace-scoped `CAP_SYS_ADMIN`, writable project/state mounts, and outer security policies permitting clone/unshare/mount. Networked builds require `/dev/net/tun` and outer networking. Persist `/home/user/.local/share` to retain images and components.

The tested Podman profile uses `--userns=keep-id:uid=1000,gid=1000`, `--user=1000:1000`, `--cap-add=SYS_ADMIN`, `--security-opt=seccomp=unconfined`, `--security-opt=label=disable`, and `--security-opt=unmask=ALL`. This is a permissive acceptance profile: a narrower policy must still permit the required operations.

Native overlay worked on the tested host without `/dev/fuse`; other kernels/drivers may require it. A nested OCI failure remains a failure. Switching to chroot would weaken the `network="none"` boundary, so Coopr does not silently do that.

## Supported limitations

- Only Linux target platforms are supported. Foreign RUNs need host `binfmt_misc` emulation; Coopr neither registers handlers nor bundles QEMU.
- Root-directory mode, ownership, and portable xattrs can be lost by the upstream image commit path even when Coopr captures them in package/cache state. The build can succeed with this limitation.
- Healthcheck and ONBUILD extensions are preserved with OCI/Docker output; receiving runtimes decide whether to honor them.
- Docker's classic store requires a selected child for multi-platform transfer. Auxiliary attestation descriptors and duplicate runnable platform entries are rejected by current image selection.
- Keyless signing is unsupported. Keyed Sigstore signatures do not include a Rekor transparency-log entry.
- Network responses, secret/cache-mount contents, and mutable host-volume/device contents are outside reproducibility guarantees and may not invalidate caches.
- Host kernels, nested runtime policies, cgroups, device access, and credential helpers remain requirements. A successful test on one host does not establish support on every CI environment.
