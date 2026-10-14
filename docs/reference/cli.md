# CLI

Use `coopr --help` to list commands and `coopr COMMAND --help` for options and defaults in the installed version. Command options appear under **Options**; inherited settings appear under **Global Options**. `coopr --version` prints the version.

## Commands

Commands below follow `coopr`. Images use the [native container store](../guides/storage.md); components use their separate OCI store.

### Images

| Command | Purpose |
| --- | --- |
| `build [file\|context]` | Build an image. Context operands require `--file FILE`. |
| `pull IMAGE...`, `image pull IMAGE...` | Pull images into native storage; default policy is `always`. |
| `push IMAGE [DESTINATION]`, `image push` | Transfer a local image; destination defaults to its registry name. |
| `tag IMAGE NAME...`, `image tag`, `copy` | Add local image names; `copy` is an alias for `tag`. |
| `save IMAGE [NAME...]`, `image save` | Save an archive or image directory; use `--multi-image-archive` for several images. |
| `load`, `image load` | Import from stdin, a file/directory, or an HTTP(S) URL supplied with `--input`. |
| `exists IMAGE`, `image exists` | Test local image presence without pulling. |
| `history IMAGE`, `image history` | Show image history, with optional platform selection. |
| `image ls [IMAGE]`, `images [IMAGE]` | List, filter, sort, or format local images. |
| `image inspect NAME...`, `inspect NAME...` | Inspect one or more local images. |
| `image rm NAME...`, `rmi NAME...` | Remove tags or images; `--all` selects all images. |
| `image prune` | Remove dangling images and unused instruction snapshots. |

### Components

| Command | Purpose |
| --- | --- |
| `component build [file\|context]` | Build a component. Context operands require `--file FILE`. |
| `component pull REF [--tag NAME]` | Pull all component platforms and optionally assign a local name. |
| `component push SOURCE DESTINATION` | Transfer a stored component to a registry or OCI archive. |
| `component tag SOURCE NAME...`, `component copy` | Add local component names; `copy` is an alias for `tag`. |
| `component save REF`, `component load` | Export/import a component OCI archive. |
| `component exists REF` | Test local component presence without pulling. |
| `component ls`, `components` | List locally named components. |
| `component inspect REF`, `component rm NAME...` | Inspect components or remove names. |

### Registry, manifests, and storage

| Command | Purpose |
| --- | --- |
| `login [REGISTRY]`, `logout [REGISTRY]` | Use the native containers authentication file and credential helpers. |
| `manifest create`, `add`, `annotate`, `inspect`, `push`, `remove`, `rm`, `exists` | Manage [image manifest lists](#manifest-lists). |
| `info` | Show host, runtime, version, and storage information; `--format` (`-f`) accepts JSON or a Go template. |
| `system df`, `system prune` | Report storage usage or prune images and unnamed component content. |
| `completion SHELL` | Generate [shell completion](#shell-completion). |

## Build controls

These controls apply to both build commands unless noted. Refer to the [execution reference](execution.md) for scope and cache rules.

Both build commands accept repeated or comma-separated values for `--platform`, `--add-host`, `--dns`, `--dns-search`, `--dns-option`, `--ulimit`, and `--decryption-key`. Image builds also accept either form for `--unsetenv`, `--unsetlabel`, and `--unsetannotation`. Options whose values contain commas, such as `--mount`, `--secret`, and `--cache-from`, take one specification per occurrence. Image `pull --decryption-key` and `push --encryption-key` take one key per occurrence.

Supply the exact definition filename, or select a definition and context separately:

```sh
coopr build image.coopr --tag app:dev
coopr build --file build/image.coopr --tag app:dev .
```

The first command uses the definition's parent directory as context; the second uses the current directory. No definition filename or `.coopr` suffix is selected automatically. `coopr component build` uses the same input rules.

| Option | Behavior |
| --- | --- |
| `--file FILE`, `-f` | Repeat to read complete stage blocks in file order with one context. Relative files are checked in the working directory first, then in the selected context when absent. Empty values are errors. |
| `--from IMAGE` | Replace the image in the first FROM instruction. |
| `--target` | Select a named output. Images default to the last top-level FROM; components default to the last top-level EXTEND. Component targets must be top-level EXTEND stages. Invocation cannot change the published target. |
| `--build-arg NAME[=VALUE]`, `--build-arg-file PATH` | Repeatable arguments/files; explicit arguments override files. Bare names import host values when set. Undeclared names produce a warning containing names only. |
| `--build-context NAME=VALUE`, `--ignorefile PATH` | Supply [named inputs](#named-inputs) or override ignore-file selection. The default is the first existing `.cooprignore`, `.containerignore`, or `.dockerignore` at the context root. |
| `--tag`, `--push` | Repeatable local names; with `--push`, those names select registry publication. |
| `--platform` | Repeated/comma-separated Linux platforms; defaults to host platform. Both build commands accept `--os`, `--arch`, and `--variant` instead; these cannot be combined with `--platform`. |
| `--jobs` | Total stage/platform concurrency; default 1, zero unlimited. |
| `--pull` | Image inputs: `missing` (default), `always`, `newer`, `never`. Bare `--pull` means always. |
| `--source-policy-file PATH` | Apply a BuildKit-format image source policy using Buildah's ALLOW, DENY, and CONVERT rules. |
| `--no-cache`, `--cache-ttl` | Bypass result reads or limit wall-clock publication age. Fresh results are still written. |
| `--cache-from`, `--cache-to` | Read or write caches; repeatable repository names without tags/digests, or `type=local,src=PATH` for reads and `type=local,dest=PATH` for writes. Supply both to read and write the same cache. |
| `--network`, `--add-host` | Default RUN network and repeatable host mappings. Authored RUN network values override the default. `--network=host` also authorizes host-network requests. |
| `--secret`, `--ssh`, `--allow` | Supply credentials or authorize elevated RUN behavior; see [security](../guides/security.md). |
| `--mount SPEC` | Add a comma-separated RUN mount specification to every RUN, after its authored mounts. Repeatable; stage sources create graph dependencies. |
| `--stdin` | Pass command stdin to RUN; cannot also read the definition or context from stdin. |
| `--source-date-epoch`, `--timestamp`, `--rewrite-timestamp` | Set creation time, force new-layer file times, or clamp newer times. Timestamp and a nonempty source date epoch are mutually exclusive. |
| `--metadata-file` | Write result/platform/destination metadata. |
| `--quiet`, `--logfile`, `--logsplit` | Suppress progress, redirect output to a file, or use one logfile per platform. See [build output](#build-output). |

### Image output controls

These options apply to image builds. Component artifacts retain their component format.

| Options | Behavior |
| --- | --- |
| `--format oci\|docker` | Select image manifest/config format; default comes from `BUILDAH_FORMAT`, otherwise `oci`. |
| `--output`, `-o` | Repeatable flattened filesystem export: `PATH`, `type=local,dest=PATH`, or `type=tar,dest=FILE` (`-` writes tar to stdout). |
| `--squash`, `--squash-all` | Compact new layers, or all layers including the base. Mutually exclusive; see [layer rules](execution.md#layers-storage-and-copying). |
| `--all-platforms`, `--manifest NAME` | Discover common base-image platforms, or append outputs to a native manifest list. |
| `--env`, `--unsetenv`, `--label`, `--unsetlabel`, `--annotation`, `--unsetannotation` | Set or remove output metadata; see [execution controls](execution.md#build-request-controls). |
| `--identity-label`, `--created-annotation`, `--inherit-labels`, `--inherit-annotations`, `--omit-history` | Control generated identity/creation metadata, inherited metadata, and instruction history. Identity, creation, and inheritance default true; history omission defaults false. |
| `--layer-label`, `--os-version`, `--os-feature` | Set intermediate-image labels and output OS requirements. Repeatable OS features ending in `-` remove that feature. |
| `--sbom[=PRESET]` | Scan with `syft` (bare flag), `syft-cyclonedx`, `syft-spdx`, `trivy`, `trivy-cyclonedx`, or `trivy-spdx`. |
| `--sbom-scanner-image`, `--sbom-scanner-command`, `--sbom-merge-strategy` | Configure a scanner image, repeatable commands using `{ROOTFS}`, `{CONTEXT}`, `{OUTPUT}`, and result merging. |
| `--sbom-output`, `--sbom-image-output`, `--sbom-purl-output`, `--sbom-image-purl-output` | Write SBOM/package-URL data to host files or paths inside the image. |
| `--sign-by`, `--sign-by-sigstore`, `--sign-by-sigstore-private-key`, `--sign-passphrase-file` | Sign images with GPG or Sigstore; native push transports validate signature support. Bare local tags support GPG signing. |
| `--cw` | Convert output for a confidential workload using native conversion options. |
| `--disable-compression` | Control native new-layer compression; default true. Explicit export compression options are described below. |

Scanner, signing, and confidential-workload options may require external tools, scanner images, or native runtime capabilities. Use `coopr build --help` for their individual parameters. Shared RUN, namespace, resource, and lifecycle flags are indexed in [execution controls](execution.md#build-request-controls).

`--iidfile PATH` writes the algorithm-prefixed native image ID, or native manifest-list ID for an aggregate output. `--iidfile-raw PATH` (alias `--raw-iidfile`) writes the image ID without its prefix and requires one platform. Neither adds a trailing newline. Either option suppresses the build's final ID line.

Image-layer compression controls are `--compression-format gzip|zstd|zstd:chunked`, `--compression-level N`, and `--force-compression`. Specifying a format defaults force-compression to true; set `--force-compression=false` to permit reuse of compatible compressed blobs. See [output compression](execution.md#output-compression) for format and storage behavior.

### Named inputs

`--build-context NAME=VALUE` accepts local directories, image references with `docker-image://`, `container-image://` or `docker://`, selected `oci-layout://PATH:TAG`, public HTTP(S) tar archives, and HTTP(S)/Git/SSH/SCP-style Git repositories.

Git query options are `branch`, `tag`, or `ref`; optional `subdir` and `checksum`/`commit`; `submodules=false`; `keep-git-dir=true`; `mtime=commit`; and `fetch-by-commit=true`. The last requires a full lowercase commit SHA in `checksum`. Nested submodules fetch by default. Legacy `#ref:subdir` remains accepted. These query controls belong to named contexts; ADD retains its instruction-level Git options.

## Names and transfer destinations

Build `--tag` and `tag` targets are ordinary names. `copy` is an alias for `tag`, including its multiple-name arguments and silent output. Inspection, removal, and `exists` look in local storage. Transport prefixes belong to transfers and image inputs, not build tags or management selectors. `--push --tag NAME` builds and publishes that name to a registry.

For an image already built as `app:dev`, assign a registry name, then publish it:

```sh
coopr tag app:dev ghcr.io/example/app:dev
coopr push ghcr.io/example/app:dev
```

Tagging adds a local name; pushing performs the transfer. Authenticate with `coopr login REGISTRY` when required. See [registry login](../guides/security.md#log-in-to-a-registry) for credentials and [registry configuration](configuration.md#runtime-and-trust) for TLS settings.

| Push destination | Images | Components |
| --- | --- | --- |
| Plain `NAME[:TAG]` | Registry image/index. | Registry artifact/index. |
| `docker://HOST/REPOSITORY:TAG` | Registry image/index. | Registry artifact/index. |
| `oci-archive:PATH[:NAME]` | OCI archive; optional name in its index. | OCI archive; optional name in its index. |
| `oci:PATH[:NAME]` | OCI image layout directory. | Unsupported. |
| `dir:PATH` | Image manifest and blobs in a directory. | Unsupported. |
| `docker-archive:PATH[:NAME]` | Docker archive; optional image name. | Unsupported. |
| `docker-daemon:NAME` | Docker Engine transfer. | Unsupported. |
| `containers-storage:REFERENCE` | Native container storage, including an explicit driver and storage root. | Unsupported. |

Image and manifest push destinations use the upstream image transports. A transport must support the requested manifest format, index, compression, and signatures; unsupported combinations report the native error. Push sources come from local storage; use `pull` or `load` to import an image first.

Whole stored-index pushes to `dir:`, `oci:`, `docker-archive:`, and `containers-storage:` use native signature handling. Coopr rejects artifact-index exports before copying when the native source cannot expose every stored index-root signature. Destinations that cannot store signatures also reject signed exports. Use `--remove-signatures` to discard signatures from the output while leaving the stored source unchanged.

`component rm NAME...` removes local names; it does not remove artifacts by digest.

Pull sources default to a registry. Image pull accepts native image transports, including `docker-daemon:NAME` for a Docker Engine image; component pull accepts a registry name or `docker://` reference. Use `load` for saved component archives.

Image pull selects one platform with `--platform OS/ARCH[/VARIANT]`, defaulting to Linux and the host architecture. Use `--os`, `--arch`, and `--variant` instead to override individual fields; none can be combined with an explicit `--platform`. These selectors also apply to `--all-tags`.

Use a digest to select immutable content for a build or export. Image `tag` adds names to the existing native record. Component names use ordinary `NAME[:TAG]` syntax and default to `latest`. Invocation checks the component store first and resolves a missing name from a registry. Local component source files use an explicit path prefix, such as `./components/settings.coopr`, with an optional `#target`; see [local components](../tutorials/local-components.md).

See [storage](../guides/storage.md) for platform selection and digest preservation. A failed post-build push reports the retained result; retry with `coopr push` or `coopr component push`.

## Archives and directories

Image and component `save` write archive bytes to stdout unless `--output FILE` is supplied. To save a complete image or stored index as an OCI archive and load it again:

```sh
coopr save --format oci-archive --output app.oci.tar app:dev
coopr load --input app.oci.tar
```

Image `save --format` accepts:

| Format | Output |
| --- | --- |
| `docker-archive` (default) | Docker image archive. Operands after the first image are additional tags; `--multi-image-archive` (`-m`) selects multiple images instead. |
| `oci-archive` | OCI archive containing one image or a complete stored index. |
| `docker-dir`, `oci-dir` | Image directory; requires `--output DIRECTORY`. |

`--compress` applies to Docker directory output; `--uncompressed` controls OCI output. Native image export may change compression and manifest/index digests while retaining filesystem layers and their order. A build's `--output` exports a [flattened filesystem](../guides/builds.md#export-an-image-or-its-files), without an image's configuration or layer structure.

Image `load --input` accepts a local archive, image directory, or HTTP(S) archive URL; omission reads stdin. Loading retains configuration and layers, including distinct members that share a platform in an OCI index. Component archives retain the complete component index and its packages. Loading a named component archive restores its name; `component load --tag NAME` overrides it. Pulling a component without `--tag` prints its locally usable digest.

Image loading imports runnable platforms. When an image descriptor omits its optional platform, Coopr reads it from the verified image configuration and retains the original descriptor. Non-image artifacts and `unknown` platforms, such as BuildKit attestations, are omitted from the stored index; its digest changes when descriptors are removed. Runnable-only indexes retain their original metadata in native storage. Export updates member descriptors if their layer representation changes.

## Manifest lists

`manifest remove LIST DIGEST` removes one member; `manifest rm LIST...` deletes lists. `manifest push` defaults to `--all=true`; false publishes the index alone. `manifest create` and `add` use `--all` to include every member of an input index.

Manifest members accept ordinary image names and explicit image transports, including `docker-daemon:`, `docker-archive:`, `dir:`, `oci:`, and `oci-archive:`. `manifest annotate LIST IMAGE` can resolve these same references to an instance digest. Ordinary names check local storage before registry resolution; a name such as `docker:dev` is an ordinary image name, while `docker://` explicitly selects a registry.

Create accepts repeatable `--annotation KEY=VALUE` for index annotations; `--amend` adds to an existing local list. Add and annotate accept instance annotations and `--os`, `--arch`, `--variant`, `--os-version`, `--os-features`, and `--features`. Annotate selects a member by image name or digest; `--index` applies annotations to the index, and `--subject` sets its subject.

`manifest add --artifact LIST FILE...` inserts the files together as one OCI artifact manifest. `--artifact-type`, `--artifact-config FILE`, `--artifact-config-type`, `--artifact-layer-type`, `--artifact-subject`, `--artifact-exclude-titles`, and repeatable `--artifact-annotation KEY=VALUE` control the artifact; ordinary `--annotation` controls its index entry. Export the complete artifact list with `manifest push --format oci LIST oci:/path/to/layout:tag` or `oci-archive:/path/to/archive:tag`. Config files retain their exact bytes, including zero-byte content.

```sh
coopr manifest create app-artifacts
coopr manifest add --artifact --artifact-type application/vnd.example.report.v1 \
  --artifact-layer-type text/plain --artifact-annotation report=summary \
  --annotation role=report app-artifacts report.txt
coopr manifest push --format oci app-artifacts oci:./app-artifacts:latest
```

The input file must exist. `--artifact-annotation` sets manifest metadata; `--annotation` sets the member’s index metadata. Artifact-specific flags require `--artifact`. Layer title annotations use filenames unless `--artifact-exclude-titles` is set. An explicit `--artifact-type=` suppresses the artifact type.

Image and manifest publication support GPG signing with `--sign-by` and Sigstore signing with `--sign-by-sigstore-private-key` or `--sign-by-sigstore PARAMETER_FILE`. Native push transports validate signature support and signing identity; named `containers-storage` destinations support signatures, while unnamed directory and OCI destinations cannot supply a Docker signing identity. Bare local tags retain the existing GPG-only signing behavior.

Parameter files use the native containers/image Sigstore format for private keys, Fulcio/OIDC and Rekor settings. A parameter-file passphrase file takes precedence over the fallback `--sign-passphrase-file` or `COSIGN_PASSWORD`. Multiple signing modes can be combined; an explicit `--sign-passphrase-file` can select at most one of `--sign-by`, `--sign-by-sq-fingerprint` and `--sign-by-sigstore-private-key`. The Sigstore environment password does not supply a GPG passphrase. Quiet mode retains required authentication instructions and interactive input.

Sequoia-PGP signing automatically discovers the optional `coopr-sequoia-signing` package beside Coopr, under `libexec/coopr`, or on `PATH`. Install that package to use `--sign-by-sq-fingerprint`; the static Coopr package keeps its existing host-GPG and Sigstore support. The optional helper owns the complete native transfer, including manifest transformations and signatures. Missing or incompatible helpers fail Sequoia requests without publishing unsigned output. Key fingerprints use the user-managed Sequoia key store; credentials and password bytes cross the helper boundary only through private temporary files.

```sh
nix profile install github:strio-dev/coopr#coopr-sequoia-signing
# From a local Coopr source checkout:
nix profile install path:.#coopr-sequoia-signing
```

Manage keys with `sq` in your Sequoia key store, then pass the signing key fingerprint to `--sign-by-sq-fingerprint`. Sequoia signing and native `signedBy` policy verification during these helper-routed transfers use the optional backend. Verification in other static Coopr operations continues to use the existing GPG backend.

The optional backend currently pins `podman-sequoia` 0.4.0. Passphrase-protected keys support single-image signing, but reusing one during a multi-image transfer can fail on the second signature with `Device is already unlocked`. Coopr preserves this upstream error; protected multi-image signing is a known limitation.

`manifest inspect` checks the local list before fetching a registry manifest, with registry authentication and TLS options. `manifest rm --ignore` tolerates missing lists; `manifest push --rm` removes the local list only after a successful push. Registry options use the same authentication, TLS configuration, and retry settings as builds, pull, and push.

## Image reports

```sh
coopr images
coopr inspect --format json app:dev
coopr history app:dev
```

`images [IMAGE]` accepts a name selector. Options include `--quiet`, `--digests`, `--history`, `--no-trunc`, `--noheading`, `--all`, `--sort created|id|repository|size|tag`, and repeatable native `--filter KEY=VALUE`. `--all` includes intermediate images; Coopr's private cache names remain hidden. `--history` adds image name history to the default table, excluding the first entry when it matches the row’s current name; empty history displays `<none>`. `--format json` returns structured data with raw name history; Go templates select fields.

Image inspection accepts multiple names and `--format` (`-f`) with JSON or a Go template. Successful inspections are printed even if another operand fails. Default output is a JSON array, including for one image. Inspecting an index selects the host platform and returns that image's metadata; use `manifest inspect` for the raw index and its members. A foreign single-platform image can still be inspected.

History uses readable ages and sizes by default. `--human=false` prints absolute dates; `--format json` retains the underlying history data, and Go templates can select individual fields.

## Transfer output and errors

### Build output

Build progress goes to stderr. A successful image build prints its native image or manifest-list ID without an algorithm prefix to stdout. Component builds print their local reference or digest. Progress includes `STEP i/n`, checkpoint IDs, `--> Using cache`, numbered stages, platform/component prefixes, final `COMMIT`, and `Successfully tagged` for named image outputs.

| Option | Output behavior |
| --- | --- |
| `--quiet`, `-q` | Suppress progress, including progress written to a logfile. The final result remains. |
| `--logfile PATH` | Write progress and the final result to the file instead of the terminal. |
| `--logsplit` | Split logfile output into `PATH_OS_ARCH[_VARIANT]` files. |
| `--iidfile`, `--iidfile-raw` | Write the image ID to a file and suppress its final line, including in logs. Image builds only. |
| `--output type=tar,dest=-` | Write the tar archive to stdout and the final image ID to stderr. Image builds only. |

Global `--log-level` controls diagnostic stderr logs: `trace`, `debug`, `info`, `warn` (or `warning`), `error`, `fatal`, `panic`. Default `warn` shows warnings/errors.

### Transfer output

| Command | Successful stdout | Progress |
| --- | --- | --- |
| `pull` | Image IDs. | stderr; `--quiet` suppresses it. |
| `push`, `manifest push` | None; `--digestfile PATH` saves the pushed digest. | stderr; `--quiet` suppresses it. |
| `component pull`, `component load` | Local component name, or digest when unnamed. | No `--quiet` option. |
| `component push` | Registry digest reference or archive path. | No `--quiet` or `--digestfile` option. |
| `tag`, `component tag` | None. | None. |

`pull --all-tags REPOSITORY` pulls every repository tag. When given several images, image pull attempts each and reports failures after successful results.

Image push accepts `--format oci|v2s2|v2s1`; manifest push accepts `oci|v2s2`. Both support compression format/level/force options and `--remove-signatures`; manifest push also accepts `--add-compression` for additional compressed variants. Image push supports repeatable `--encryption-key` and `--encrypt-layer`, whose negative indexes count from the last layer. Specifying compression format defaults force-compression to true; explicit `--force-compression=false` permits compatible blob reuse. Omitted format/level options use native `containers.conf` defaults.

Image push `--compress` compresses layers for `dir:` destinations. It does not select a compression algorithm or force recompression of an existing variant. Coopr does not expose Buildah’s optional blob-cache compression selector. Build `--disable-compression` has a separate [new-layer meaning](#image-output-controls).

### Exit status

Command errors go to stderr with an `Error:` prefix and normally return 125. Option-parsing errors also print the command's help hint.

| Command | Exit status |
| --- | --- |
| `exists`, `component exists`, `manifest exists` | Silent: 0 for present, 1 for absent, 125 for storage errors. |
| Build with a failed RUN | The RUN command's exit status. |
| `rmi`, `image rm` | 1 for missing-only failures, 2 for images in use, 125 for other failures. Successful removals are still reported. |

Image removal reports distinguish `Untagged:` names from `Deleted:` image IDs.

## Shell completion

Generate a completion script with `coopr completion bash`, `zsh`, `fish`, or `powershell`. `--file` (`-f`) writes it directly to a file; `--no-desc` omits descriptions. For the current Bash session:

```sh
source <(coopr completion bash)
```

Install persistent scripts in your shell's completion directory. Completion omits hidden options and avoids filename suggestions for registry usernames and passwords.

## Configuration and lifecycle

Global storage/runtime settings live in [configuration](configuration.md). Image pruning follows Podman's image rules: the default retains named images, images used by containers, and intermediate layers needed by another image or manifest list.

| Command | Removes |
| --- | --- |
| `image prune` | Dangling images and unused instruction snapshots. |
| `image prune --build-cache` | The same, plus persistent RUN cache mounts. |
| `image prune --all` | All unused images, including named images, and RUN cache mounts. |
| `system prune` | Dangling images, unused instruction snapshots, and unnamed component content. |
| `system prune --all` | All unused images, RUN cache mounts, and unnamed component content. |

`system prune` does not manage Podman containers, pods, networks, or volumes. Only `image prune` accepts `--build-cache`.

Both prune commands ask for confirmation before removing data; `--force` (`-f`) skips the prompt. `--dry-run` previews initial image candidates without changing data or asking for confirmation.

Repeatable `--filter label=KEY[=VALUE]`, `label!=KEY[=VALUE]`, and `until=TIME` restrict image candidates; filters do not select component blobs or RUN mount directories.

`system df --verbose` shows per-image usage, and `--format json` or a Go template formats the summary; format and verbose cannot be combined. Recursive pruning can remove additional parent images. See [storage](../guides/storage.md#inspect-and-maintain) for examples and cache-mount scope.

Build-container cleanup, saved stages, volume compatibility, and `--layers` are covered in the [execution control index](execution.md#build-request-controls). These settings apply to both build commands.
