# Security and rootless builds

Coopr embeds Buildah and can build as your regular Linux user. Review definitions and components before running them: their instructions execute code with the privileges available to Coopr.

## Native rootless requirements

Configure your host with [Podman's rootless setup](https://github.com/podman-container-tools/podman/blob/main/docs/tutorials/rootless_tutorial.md). Coopr needs:

- User and mount namespaces, subordinate UID/GID ranges, and working `newuidmap` and `newgidmap` helpers.
- Supported container storage and an OCI runtime such as `crun`. Overlay storage may need `fuse-overlayfs`.
- The network helpers selected by your native configuration for networked RUNs.

Memory and CPU limits need cgroup v2 with the required controllers delegated to your user. Unsupported limits fail explicitly.

Run Coopr and Podman as the same user with the same storage settings to share images. See [host setup](../getting-started/index.md#set-up-your-host) for a basic check.

## Embedding in an application

The [Go library](library.md) uses Buildah's same-executable helper dispatch before
normal application argument checks. Those helpers are reachable independently
of the application's CLI. A setuid installation, executable file capabilities,
or a restricted sudo rule can therefore expose more operations than that CLI
permits. This also applies when a launcher or SELinux domain transition grants
additional permissions. Coopr does not provide a privilege boundary for such an
application.

Run the engine under the account whose permissions builds should have, normally
rootless. Root and sudo builds grant the engine the operator's host authority.
Authenticate service requests before submitting them to the library; container
isolation does not replace application access control.

## Credentials and remote sources

### Log in to a registry

```sh
coopr login docker.io
```

Coopr prompts for credentials and uses the credential file and helpers shared with Podman and Buildah, for both images and components.

Use `--authfile PATH` to select another file, or `coopr logout docker.io` to remove the login.

For automation, use `--username USER --password-stdin` rather than passing passwords in command-line arguments.

### Mount a build secret

Supply credentials with a secret mount rather than an ARG, ENV, or copied context file. The mount is temporary; commands can still expose its contents through logs or files they write.

Save this as `image.coopr` to check that a secret is available without printing it:

```kdl
from "docker.io/redhat/ubi9:latest" {
    run {
        script "test -s /run/secrets/token"
        mount "secret" id="token" target="/run/secrets/token" required=#true
    }
}
```

With a nonempty token file outside the context at `../token`, build with:

```sh
coopr build image.coopr --secret id=token,src=../token
```

The ID connects the CLI input to the mount. `required` fails an executing RUN if the secret is missing. To read an existing environment variable instead, use `--secret id=token,env=TOKEN`.

Secret values and SSH agents do not invalidate cached results. A cache hit skips the RUN, including its required-secret check. Use `--no-cache` when changed credentials or their availability must affect execution. See [refresh rules](caching.md#refresh-deliberately).

### Forward SSH and authenticate remote inputs

Supply `--ssh default` to forward `SSH_AUTH_SOCK` through a RUN's [SSH mount](../reference/definition.md#run-mount-options). This grants the build access to the agent. Configure host-key verification inside the image for commands such as `git` or `ssh`.

Remote inputs fetched outside RUN use separate credential inputs:

- **HTTPS Git:** host-scoped `GIT_AUTH_HEADER.<host>` or `GIT_AUTH_TOKEN.<host>` secrets. Authenticated redirects are rejected; out-of-scope submodules receive no parent credentials.
- **SSH Git:** an SSH source and `GIT_KNOWN_HOSTS` or `GIT_KNOWN_HOSTS.<host>` secret for host-key verification.
- **HTTP ADD:** host-scoped authorization secrets, selected separately for each redirect host.
- **Registry images, components, and caches:** native registry credentials, certificates, TLS settings, and retry options.

See [remote inputs](../reference/definition.md#remote-inputs) for exact secret names and [configuration](../reference/configuration.md#runtime-and-trust) for registry settings.

## Grant additional RUN access

Entitlements are checked even on cache hits. Grants cannot supply privileges missing from the host or outer container.

- **Host networking:** `run network="host"` requires `--allow network.host`. Build-wide `--network=host` both selects host networking and grants this permission.
- **Insecure execution:** `run security="insecure"` requires `--allow security.insecure` and OCI/rootless isolation. Chroot isolation is rejected.
- **CDI devices:** a RUN `device` child requires `--allow device` or a matching `--allow device=SELECTOR`, unless the CDI metadata authorizes the device.

## Trust images and components

Pin image and component references by digest for fixed content, including external inputs inside components. A digest identifies bytes, not a trusted publisher; a component pin does not pin mutable inputs in its definition.

Image inputs honor native registry routing and `policy.json` signature policy; this does not verify component signatures. Keep TLS verification enabled. See [runtime and trust configuration](../reference/configuration.md#runtime-and-trust).

Image publication supports GPG and Sigstore signing. Sigstore parameter files use the native containers/image format, including private keys, Fulcio/OIDC keyless signing and optional Rekor transparency-log settings. Direct private-key signing does not add a Rekor entry automatically. See [signing options](../reference/cli.md#manifest-lists) for authentication and passphrase handling.

## Nested container profile

The published scratch-based image includes Coopr and its build helpers, without a distribution package manager or builder daemon. UID/GID 0 inside rootless Podman maps to your host user.

The [container build command](container.md#build-an-image) exposes `/dev/fuse`, mounts the project, persists `/var/lib`, and disables seccomp filtering and SELinux labeling for that container. Images, components, and caches stay in the volume; storage uses `fuse-overlayfs`.

The default `BUILDAH_ISOLATION=chroot` shares the outer container's network, IPC, PID, and cgroup namespaces. Apply network restrictions and resource limits to outer Podman. Chroot rejects requests for `none`, private, or named RUN network namespaces, including build-wide `--network=none`, before execution or cache reuse.

Separate RUN network namespaces or cgroup controls require `--isolation=rootless` or `--isolation=oci` on `coopr build`. Place Coopr options after the image name when using Podman. Nested OCI execution needs user/mount namespaces, UID-map helpers, and outer policies permitting clone/unshare/mount. Network helpers may need `/dev/net/tun`; resource limits need cgroup delegation. OCI failures do not trigger a chroot fallback.

## Supported limitations

- Foreign-architecture RUNs need host `binfmt_misc` emulation. Coopr supports Linux targets and neither registers emulators nor bundles QEMU.
- Root-directory mode, ownership, and portable xattrs may be lost by upstream image commits, even though package/cache snapshots capture them. See [filesystem metadata](../reference/execution.md#state-identity-and-metadata-limits).
- Healthcheck and ONBUILD extensions are preserved with OCI/Docker output; receiving runtimes decide whether to honor them.
- Docker's classic store requires a selected child for multi-platform transfer. OCI archive loading omits non-image artifacts and `unknown` platforms; an omitted image descriptor platform is read from its verified configuration. See [image transfers](../reference/execution.md#layers-storage-and-copying) and [archive loading](../reference/cli.md#archives-and-directories).

Context and image archives have no size quota: expansion can exhaust disk, memory, or CPU. Use trusted sources and bound the Coopr process or its outer container when needed. RUN resource options do not limit input preparation. Kernel features and runtime policy also determine which CI environments can run Coopr.
