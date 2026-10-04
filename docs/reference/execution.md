# Execution

Definitions contain ordered KDL v2 instructions in `.coopr` files. A definition without `extend` builds an image; one with `extend` builds a component. See [definition syntax](definition.md) for authored forms, [CLI](cli.md) for flags, and [storage](../guides/storage.md) for stores and transfers.

## State and stages

A stage has a Linux filesystem, image configuration, platform, and argument scope. Its instructions run in source order.

| Stage source | Initial state |
| --- | --- |
| `from "IMAGE"` | Selected immutable OCI image and its configuration. |
| `from "scratch"` | Empty filesystem and configuration. |
| `from "STAGE"` | Named stage's completed filesystem, configuration, platform, and declared arguments. |
| `package as="NAME"` | Empty filesystem and configuration; produces a stored package snapshot. |
| `extend` | Consumer's current filesystem and configuration. Multiple roots start independently from that same state. |

The final stage is the image output unless `--target` selects another. A component output must descend from `extend` through FROM links. Component build fixes that output; an invocation property named `target` is an ordinary argument.

A component call replaces the caller's filesystem and configuration, then the caller continues. Caller arguments remain in scope; component-private arguments do not. Returned ENV values win collisions with caller arguments. A later caller ARG declaration still uses the original caller CLI override. Like FROM, a component call makes a later ENTRYPOINT clear inherited CMD unless a subsequent CMD supplies one.

COPY/ADD and RUN mounts with `from=` create input dependencies without changing base lineage. Sources can be named stages, packages, named contexts, or external images. External images resolve by platform to immutable descriptors. Graphs must be acyclic and stage aliases unambiguous; graph-affecting arguments must resolve before execution.

Published components fix whether each FROM, COPY/ADD, and RUN mount source is a stage, `scratch`, or an external image. Invocation cannot change that role or a stage dependency. Arguments may change an external reference while it remains external. Component references themselves may use caller ARG/ENV values resolved at invocation. If a deferred component could change the scope needed to classify a dynamic COPY or mount source, publication rejects the unknown role; literal sources remain usable.

### Platforms

Only Linux platforms are supported. Repeated or comma-separated `--platform` values plan independent builds; a FROM `platform=` can select another Linux platform for that stage. The output stage determines the actual output platform. Duplicate normalized output platforms fail.

One image platform produces a manifest; several produce an OCI index, or a Docker manifest list with `--format docker`, preserving requested order. Component builds similarly produce one manifest or an OCI index of complete per-platform component artifacts. Invocation selects the child matching the consumer platform.

Executing foreign-architecture binaries requires host-provided `binfmt_misc` emulation. Coopr does not register emulators. Filesystem and metadata operations need no emulation when they do not execute target binaries.

### Inherited ONBUILD

FROM runs inherited triggers in stored order before authored instructions, then clears them. Local COPY/ADD and bind mounts use the child build's context; inherited secret/SSH mounts use the child's supplied sources. EXTEND does not consume the caller's triggers.

Planning binds inherited ARGs and stage references before execution. Publication retains earlier named stages and package snapshots that invocation-time triggers could activate. Invocation-only external bases resolve at invocation and remain pinned for execution. Inherited stage references must name retained earlier stages.

## Component build and invocation phases

| Phase | Executes | Local input context |
| --- | --- | --- |
| Image build | Selected output and dependencies. | Image caller's context. |
| Component build | Required package producers and dependencies. | Component builder's context. |
| Component invocation | Retained invocation graph, using stored packages as leaves. | Consuming caller's context. |

Package producers cannot depend on EXTEND: the consumer filesystem is unavailable at publication. Unneeded packages are excluded, except retained snapshots needed by possible inherited triggers. A shared private producer may run in both phases, using each phase's context. Producer instructions used only at publication are omitted from the invocation graph; directly referenced producers remain executable there.

Packages store both filesystem and full image configuration. FROM inherits both; COPY/ADD takes files only. Referencing a package does not otherwise install its configuration or append its layers to the consumer. Component-owned files needed at invocation must be captured in a package; invocation does not require the source checkout.

Package imports verify content and the full Linux platform, including OS version/features. Filesystem metadata remains subject to the [output limitations](#state-identity-and-metadata-limits).

A component retains its selected output, definition, fixed arguments, and package snapshots. Multi-platform builds package each platform separately. Architecture matching alone does not prove distro or package-manager compatibility.

### Compatibility requirements

An EXTEND field accepts one string property or one child containing allowed strings:

```kdl
extend as="base" {
    distro "fedora" "rhel"
    distro-version "41" "42" "9"
    package-manager "dnf" "yum"
    architecture "amd64" "arm64"
}
```

Each field may appear once; lists require nonempty strings. Values support global ARG expansion, and architecture aliases are accepted.

Any value within a field may match. Every declared field on every retained EXTEND root must match before body execution or component-cache reuse.

| Requirement | Match rule |
| --- | --- |
| `architecture` | Normalized selected Linux OCI architecture. |
| `distro` | Exact `ID` from `/etc/os-release`; `/usr/lib/os-release` is used only when the first file is absent. No `ID_LIKE` inference. |
| `distro-version` | Exact `VERSION_ID`; requires `distro`. Omission permits any allowed distro version. No version ranges. |
| `package-manager` | An allowed executable at an absolute path or a simple name in the image's absolute PATH directories. Conventional PATH defaults apply when absent. No manager-version check. |

Missing or conflicting required values fail. Filesystem-based checks may require solving the caller before that boundary; unrelated stages remain schedulable.

## Instructions and image configuration

The [syntax reference](definition.md#instructions) lists supported instructions and command forms. Unsupported instructions/options fail explicitly. Coopr expands component parameters and structural properties, including SHELL and MAINTAINER values.

Configuration-only instructions change stage state without changing the filesystem. Coopr preserves image configuration that the instructions do not modify. Filesystem metadata remains subject to the output format and runtime limits.

### RUN network and devices

The build-wide `--network` supplies RUN's default. An authored value, including `network="default"`, overrides it for authored, inherited, and component RUNs. Modes include default, private, none, host, named networks, `ns:PATH`, and Pasta/slirp4netns options. Effective modes determine validation and cache identity; `network="none"` rejects DNS controls.

| Elevated operation | Authorization and limit |
| --- | --- |
| `network="host"` | Requires `--allow network.host`, including cache hits. In containerized Coopr, host means the outer container's network namespace. |
| `security="insecure"` | Requires `--allow security.insecure`, including cache hits. Requires OCI/rootless isolation; chroot fails. Applies the insecure OCI spec policy, bounded by outer container, user namespace, and kernel permissions. Writable sysfs/cgroup remounts may fail in nested rootless containers; failures are reported. |
| RUN `device` children | Require `--allow device`, `--allow device=SELECTOR`, or CDI auto-allow annotation. Selection/authorization precede cache lookup. Only devices/CDI files available to the outer container can pass through. |

For example, `run "command" { device "vendor.example/gpu=card0" required="true" }` requests a CDI device. Repeatable `--add-host 'HOST[;HOST...]:IP'` applies to all RUNs and cache identities. `host-gateway` is accepted, but its host-dependent resolution disables RUN instruction and component/package result caching. See [security](../guides/security.md).

## Arguments and normalization

| Scope | Rule |
| --- | --- |
| Global ARG | Declared before the first stage; available for structural expansion. Redeclare in a stage to expose it to RUN. |
| Stage ARG | Applies from declaration; inherited through FROM. |
| Independent EXTEND | Gets global and explicitly supplied component arguments plus caller image ENV, without caller-local or sibling-root ARGs. |
| Package arguments | Every argument in the producer's effective scope is fixed at publication, even if unused in command text. Invocation overrides fail. |
| Invocation arguments | Adjustable after EXTEND unless also fixed by package production. |

Values are strings and parameters require valid UTF-8. Unset/undeclared ARG references expand to empty text; unused supplied build arguments are ignored. Structural references must remain valid after expansion. Conflicting package values fail. Standard ARG/ENV substitution, shell/exec boundaries, and RUN exposure retain the supported Containerfile behavior.

### Timestamps and cache age

`SOURCE_DATE_EPOCH` accepts a supplied Unix timestamp or global ARG default; empty clears it. It controls image/history creation and reaches RUN only after stage redeclaration. `--source-date-epoch` overrides the resolved value. An explicit numeric build argument overrides an authored `context` default.

| Epoch source | Value |
| --- | --- |
| `context` | Primary Git committer time; HTTP Last-Modified, otherwise newest regular-file archive mtime; unset for local context. |
| Named context | Same Git/HTTP rules; unset for local directory, image, or OCI layout. |
| Source-only stage name | Remote ADD metadata. Stage must use FROM scratch, ARGs, and exactly one HTTP(S)/Git ADD. |

`--timestamp` forces creation and new-layer file times, including older files and package snapshots. It conflicts with explicit `--source-date-epoch` and `--rewrite-timestamp`. `--rewrite-timestamp` clamps newer layer/package file times to the epoch. Timestamp policies enter cache identities.

`--cache-ttl DURATION` limits reads by wall-clock publication age, independently of image timestamps. Zero disables reads while permitting fresh writes.

## Build contexts and local files

Definition and primary context are independent inputs. Either can use stdin, but both cannot do so in one request. Contexts accept directories, local tar archives, stdin tar, Git checkouts, or HTTP(S) tar archives. `--file` also accepts an HTTP(S) definition. Definitions selected inside remote/archive contexts must remain inside them after symlink resolution; explicit local definitions may live outside the context.

Ignore selection is `--ignorefile`, otherwise the first context-root file: `.cooprignore`, `.containerignore`, `.dockerignore`. Docker/Podman patterns filter local COPY/ADD, RUN binds, and local component definitions; filtered inputs enter cache identity. Definition placement does not change ignore selection. Paths/symlinks cannot escape the selected context.

Local contexts exclude host `security.selinux` labels. Other xattrs, including `security.capability` and `user.*`, remain subject to runtime and exporter limits.

### Named contexts

Names take precedence over image references for FROM, COPY/ADD `from=`, and RUN mount `from=`. A matching stage alias replaces that stage and skips its body; numeric references still select stages. Only selected-graph contexts materialize. Local named contexts use their own ignore files and freeze contents before execution. Component build contexts apply to package production; invocation local inputs come from the caller.

For transport syntax, Git selectors, and credentials, see [named inputs](cli.md#named-inputs) and [remote inputs](definition.md#remote-inputs).

## Build request controls

See [CLI build controls](cli.md#build-controls) for option syntax and [security](../guides/security.md) for host requirements. `--jobs` limits total stage/platform work (default 1; zero unlimited) without changing dependency order or cache identity.

RUN resource, namespace, and runtime settings affect cache reuse; host volume/device contents are not measured. Memory and swap apply independently; explicit shared-memory zero differs from omission. Host permissions and cgroup delegation can reject resource requests.

Image `--env` prepends values to every stage; authored ENV wins. Bare names import present host values, `PREFIX*` imports matching names, and `*` imports all. Repeated context names and secret/SSH IDs use the last value. Label/environment unsets, inheritance, and history omission apply to final outputs, including cache hits. OCI annotations are dropped with Docker format.

`--all-platforms` discovers common runnable Linux platforms across non-scratch bases and conflicts with explicit `--platform`. `--manifest NAME` serializes updates to a named local index, preserves other platforms, and replaces newly built instances.

### Outputs and failure recovery

Repeatable `--tag` applies local names or transfers to one retained immutable result, in requested order. Plain names are local; explicit transports are `local:`, `registry:`, `oci-archive:`, `podman:`, and `docker:`. Components support only the first three. `--push --tag NAME` selects registry publication.

`--metadata-file` records `containerimage.digest`, `containerimage.config.digest`, `containerimage.descriptor`, `coopr.platforms`, `coopr.references`, and per-destination `coopr.outputs`. Image `--iidfile` writes the config image ID for one platform or index digest for several. File-output parents are checked before execution.

Successful execution retains the result/checkpoints before transfers. Destinations commit independently; there is no cross-engine/registry transaction. Transfer failure/cancellation reports the retained digest and completed destinations. Metadata records complete/failed/pending destinations and `coopr.outputError`; result-file finalization failures also report retained outputs. An interrupted transfer may have committed remotely: check its destination, then retry with `coopr copy`. Execution failure publishes neither newly staged portable-cache candidates nor final output names.

Registry resolution, caches, and publication share the request's [authentication and trust configuration](configuration.md#runtime-and-trust).

## Resolution, compatibility, execution, and caching

### Image selection

Registry component tags resolve on each build. Image tags default to local cached selection. `--pull-policy` accepts `missing` (default), `always`, `newer`, and `never`; bare `--pull` means always. `newer` compares selected digests and can reuse a local image on registry failure. `never` fails missing inputs before registry access. Digest references select exact objects and can reuse local copies offline. Reproducibility requires pinning every selected OCI reference, including nested ones.

### Cache scopes

Reuse requires matching input state, arguments, platform, execution settings, and declared sources.

| Cache | Reuses | Limits |
| --- | --- | --- |
| Local instruction | RUN, COPY, ADD, and WORKDIR checkpoints across image and component phases. | Metadata-only changes need no filesystem checkpoint. |
| Portable instruction | Instruction checkpoints from an OCI cache. | Unpinned remote ADD and device-backed RUN stay local. Networked RUN and declared mounts can reuse results. |
| Component state | The selected component output. | Caller-local inputs without complete identities prevent whole-component reuse. |
| Package result | Completed package snapshots. | All selected package results must be available; otherwise their producers execute. |
| Cache mount | Disposable tool data. | Names are scoped to their use unless explicitly supplied. Builds must work with empty mounts. |

COPY/ADD measure their selected inputs. Checksum-pinned HTTP ADD can hit without downloading again; Git ADD resolves the commit and submodules before checking the cache. A cache hit therefore does not guarantee an entirely offline build.

Package-result caching supports networked RUN, bind/cache/tmpfs/secret/SSH mounts, and checksum-pinned remote ADD. Unpinned remote ADD, nested components, RUN networks other than default/none, and insecure/device RUN bypass whole-package reuse; eligible instructions can still hit.

Network responses, credentials/SSH-agent state, cache-mount contents, clock, randomness, and undeclared host inputs do not invalidate conventional cache results. A cache hit is not a reproducibility guarantee. Required reusable files belong in packages. Cache repositories are trusted inputs and may contain intermediate files absent from final images; protect them accordingly.

`--no-cache` bypasses instruction, component-state, and package-result reads, writes fresh results, and leaves cache mounts intact. Repeatable `--cache` enables reads/writes; `--cache-from` reads only; `--cache-to` writes only. Transports are `oci-layout:PATH` and `registry:HOST/REPOSITORY`. Validated hits can seed another writable destination; read-only sources are never updated and write-only destinations are never queried.

### State identity and metadata limits

Portable cache identity follows effective filesystem and image-configuration changes, independently of layer history. It ignores the root directory’s modification time. Metadata that the exporter cannot preserve can prevent portable reuse.

Ordinary image commits have a Buildah/Podman limitation: changes to root-directory mode, owner, or portable xattrs may be discarded at layer commit. Coopr accepts such builds even when committed images cannot represent them. Package snapshots and cache checkpoints capture/restore these root attributes. Portable state snapshots cannot reuse outputs whose metadata the exporter cannot preserve, including subsecond file mtimes and nonportable xattrs.

### Layers, storage, and copying

Default `--format oci` preserves original base layers/configuration; Docker format selects schema 2 config/manifests and manifest lists. Ordinary filesystem changes produce one layer per instruction; component invocation produces one net change layer. Configuration-only/empty changes add none. `--layers=false` combines newly executed filesystem instructions into one layer. Packages are invocation inputs, not appended consumer layers.

Without a tag, builds return a manifest/index digest; local tags return their name. See [storage](../guides/storage.md) for stores and transfers.

`coopr copy` accepts a local tag/digest or explicit engine source. Complete locally built indexes copy as complete indexes by default; `--platform` selects one child. Docker's classic store requires that selection; its containerd store accepts indexes, with capability/digest/platform checks. Engine-imported indexes must contain exactly one runnable Linux manifest per platform; auxiliary descriptors and duplicate platforms fail. Engine conversion may change representation; `--format docker` avoids relying on Docker's OCI conversion.

Partially imported registry indexes default to native Linux selection. Single-platform local tags select their stored platform; bare manifest digests select that exact manifest regardless of host.

Local component invocation accepts `local:TAG` or a bare immutable digest; registry failure does not search local storage. Components are not engine-loadable images. Pruning follows [storage lifecycle rules](../guides/storage.md#inspect-and-maintain); external Podman access uses native storage locking.
