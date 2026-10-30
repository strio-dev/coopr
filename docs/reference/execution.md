# Execution

This reference defines how Coopr resolves inputs, executes stages and reuses cached results. Use [definition syntax](definition.md) to write instructions and [CLI](cli.md) to find commands and options. For worked examples, start with [Build images](../guides/builds.md) or [Package components](../guides/components.md).

## State and stages

A stage has a Linux filesystem, image configuration, platform, and argument scope. Its instructions run in source order inside the stage’s `{ ... }` block. Only global ARG declarations and stage blocks belong at the top level.

Image definitions contain FROM stages. Component definitions require EXTEND and may also contain FROM and PACKAGE stages.

| Stage source | Initial state |
| --- | --- |
| `from "IMAGE" { ... }` | Selected immutable OCI image and its configuration. |
| `from "scratch" { ... }` | Empty filesystem, default PATH, and working directory `/`. |
| `from "STAGE" { ... }` | Named stage's completed filesystem, configuration, platform, and declared arguments. |
| `package as="NAME" { ... }` | Empty filesystem with scratch stage defaults; produces a stored package snapshot. |
| `extend { ... }` | Consumer's current filesystem and configuration. Multiple roots start independently from that same state. |

Before running instructions, FROM supplies the standard Linux PATH when its base has no `PATH` entry. An explicit empty or custom PATH is preserved. EXTEND inherits the caller without adding defaults. It also continues the caller’s CMD/ENTRYPOINT evaluation: an authored CMD survives a later ENTRYPOINT across a component call. A new FROM starts a fresh stage, where ENTRYPOINT clears an inherited CMD unless that stage authors its own CMD.

### Output selection

| Build | Default output | Explicit selection |
| --- | --- | --- |
| Image | Last top-level FROM. | `--target NAME` selects a named FROM. |
| Component artifact | Last top-level EXTEND; trailing FROM/PACKAGE stages do not change it. | `--target NAME` selects a named top-level EXTEND. |
| Local component call | The file's default component output. | `./shared.coopr#configured` selects a named top-level EXTEND during automatic packaging. |

Stored and registry artifacts keep the output chosen at packaging. Nested FROM aliases supply dependencies, not callable component targets. Stage aliases match case-insensitively. The `#` separator is reserved in local component references and requires one nonempty target. A component-call `arg "target" "value"` binds an ordinary argument; it does not select an output.

Inside EXTEND, direct instructions modify the caller; nested FROM blocks own separate stages. The last non-ARG child determines the result: a nested FROM returns its image, while a direct instruction returns the modified caller. ARG does not switch the result. The selected result includes its filesystem, configuration and layers; see [returned state](#returned-state).

### Dependencies

COPY/ADD and RUN mounts with `from=` create input dependencies without changing base lineage. Sources can be named stages, packages, named contexts, or external images. COPY/ADD can also read [OCI artifact files](definition.md#copy-oci-artifact-files). External inputs resolve to immutable descriptors; image and artifact indexes select the stage's platform. Stage sources must precede their consuming FROM, COPY/ADD, or RUN mount. Nested FROM helpers become visible to later instructions inside EXTEND. Graphs must be acyclic and stage aliases unambiguous. Graph-affecting values must resolve before their consuming instruction executes; after a component returns, planning continues with its actual output environment.

Published components fix whether each FROM, COPY/ADD, and RUN mount source is a stage, `scratch`, or an external image. Invocation arguments cannot change that role or a stage dependency. Explicit [named build contexts](#named-contexts) can substitute a source without rewriting the published definition. Arguments may change an external reference while it remains external. Component references themselves may use caller ARG/ENV values resolved at invocation.

If a deferred component could change the scope needed to classify a dynamic COPY or mount source, publication rejects the unknown role; literal sources remain usable.

### Platforms

Only Linux platforms are supported. Repeated or comma-separated `--platform` values plan independent builds; a FROM `platform=` can select another Linux platform for that stage. The output stage determines the actual output platform. Duplicate normalized output platforms fail.

One image platform produces a manifest; several produce an OCI index, or a Docker manifest list with `--format docker`, preserving requested order. Component builds similarly produce one manifest or an OCI index of complete per-platform component artifacts. Invocation selects the child matching the consumer platform. Local file components are packaged for that platform. Component calls have no platform override; use `from ... platform=... { ... }` for independently based stages.

Executing foreign-architecture binaries requires host-provided `binfmt_misc` emulation. Coopr does not register emulators. Filesystem and metadata operations need no emulation when they do not execute target binaries.

### Inherited ONBUILD

FROM runs inherited triggers in stored order before authored instructions, then clears them. Local COPY/ADD and bind mounts use the child build's context; inherited secret/SSH mounts use the child's supplied sources. Native RUN mount text is expanded in child scope before its type and fields are validated. Imported triggers retain their original native field spelling through that expansion. EXTEND does not consume the caller's triggers.

Authored inline COPY/ADD bodies retain decoded KDL bytes and expand ordinary-string ARG/ENV references at their execution phase; raw bodies and filenames stay literal. ONBUILD serialization preserves child-time expansion and native filenames. Native heredocs always terminate their data with LF, including empty bodies, so authored ONBUILD inline files require nonempty LF-terminated content and reject delimiter collisions or other parsing losses. Ordinary authored inline files have no line-termination requirement. Inline batches precede ordinary sources, bypass context ignore/exclude filtering, and share the existing layer, worker and instruction-cache paths.

Planning binds inherited ARGs and stage references before execution. Publication retains named stages and package snapshots that invocation-time triggers could activate. Invocation-only external bases resolve at invocation and remain pinned for execution. Inherited stage references must select retained stages declared before the child FROM; numeric references preserve their original source identities.

## Component build and invocation phases

| Phase | Executes | Local input context |
| --- | --- | --- |
| Image build | Selected output and dependencies. | Image caller's context. |
| Component build | Required package producers and dependencies. | Component builder's context. |
| Component invocation | Retained invocation graph, using stored packages as leaves. | Consuming caller's context. |

FROM stages follow these dependencies regardless of position or nesting. A stage needed by both a package and the selected extend runs in both phases, with each phase's inputs. Stage sources must still be declared before their consumers.

A package cannot depend on actual EXTEND caller state: the consumer filesystem is unavailable at publication. An independently based FROM can supply a package, including one nested inside EXTEND. Only package snapshots are immutable leaves. Packaging-only producers are omitted from the invocation graph; unneeded stages are excluded, except dormant dependencies retained for inherited triggers. See [Stages and component phases](../concepts/stages.md) for a complete example.

Packages store both filesystem and full image configuration. FROM inherits both; COPY/ADD takes files only. Referencing a package does not otherwise install its configuration or append its layers to the consumer. Component-owned files needed at invocation must be captured in a package; invocation does not require the source checkout.

Package imports verify content and the full Linux platform, including OS version/features. Filesystem metadata remains subject to the [output limitations](#state-identity-and-metadata-limits).

A component retains its selected output, definition, fixed arguments, and package snapshots. Multi-platform builds package each platform separately. Components use the caller’s platform; they do not infer distribution or package-manager requirements.

### Returned state

A component call adopts the selected output's complete image chain, filesystem and configuration, then the caller continues. A FROM output uses its selected base's normal configuration inheritance and authored overrides. An independently based output drops caller-only files and configuration.

| State after the call | Rule |
| --- | --- |
| Caller ARGs | Remain in scope; component-private ARGs do not escape. |
| Returned ENV | Wins collisions with caller arguments. |
| Later caller ARG declaration | Retains the original caller CLI binding when supplied; returned ENV still wins when the names collide. |
| Later ENTRYPOINT | Clears inherited CMD unless a subsequent CMD supplies one, as after FROM. |

## Instructions and image configuration

The [syntax reference](definition.md#instructions) lists supported instructions and command forms. Unsupported instructions/options fail explicitly. Coopr expands component parameters and structural properties. SHELL arguments and MAINTAINER text remain literal, matching native instruction behavior.

Configuration-only instructions change stage state without changing the filesystem. Coopr preserves image configuration that the instructions do not modify. Filesystem metadata remains subject to the output format and runtime limits.

### RUN network and devices

The build-wide `--network` supplies RUN's default and supports Buildah's private, named-network, `ns:PATH`, Pasta and slirp4netns options. An authored RUN accepts `network="default"`, `network="none"`, or `network="host"`, matching Containerfile RUN. This value overrides the build-wide default for authored, inherited, and component RUNs. Argument substitutions are checked after expansion. Effective modes determine validation and cache identity; `network="none"` rejects DNS controls.

Chroot shares its caller's network namespace. It accepts default or host networking; requests for other modes fail before execution or cache reuse. Use `--isolation=oci` or `--isolation=rootless` for network namespace isolation.

| Elevated operation | Authorization and limit |
| --- | --- |
| `network="host"` | Requires `--allow network.host` or build-wide `--network=host`, including cache hits. In containerized Coopr, host means the outer container's network namespace. |
| RUN `device` children | Require `--allow device`, `--allow device=SELECTOR`, or CDI auto-allow annotation. Selection/authorization precede cache lookup. Only devices/CDI files available to the outer container can pass through. |

Request a CDI device with a child of RUN:

```kdl
run "command" {
    device "vendor.example/gpu=card0" required=#true
}
```

Repeatable `--add-host 'HOST[;HOST...]:IP'` applies to all RUNs and cache identities. `host-gateway` resolves during execution; its declared host mapping participates in cache identity. See [security](../guides/security.md) for credentials and elevated access.

## Arguments and normalization

| Scope | Rule |
| --- | --- |
| Global ARG | Declared at the top level; available for structural expansion. Redeclare inside a stage to consume it and expose it to RUN. |
| Stage ARG | Declared inside the stage block; consumes a global value or supplies a stage default. Applies from declaration and is inherited through FROM. |
| Independent EXTEND | Gets global and explicitly supplied component arguments plus caller image ENV, without caller-local or sibling-root ARGs. |
| Package arguments | Arguments used during package production are fixed at publication, including values exposed to RUN. Invocation overrides fail. |
| Invocation arguments | Adjustable in invocation stages unless also fixed by package production. |

Argument values are strings and require valid UTF-8. Unset/undeclared ARG references expand to empty text; structural references must remain valid afterward. Conflicting package values fail. See [argument syntax](definition.md#arguments) and [command forms](definition.md#command-forms) for substitution and shell/exec boundaries.

Unused supplied build arguments are ignored with a warning containing names only. Predefined proxy arguments (`HTTP_PROXY`, `HTTPS_PROXY`, `FTP_PROXY`, `NO_PROXY`, `ALL_PROXY`, and lowercase forms) can reach RUN without ARG declarations and do not enter its argument/cache identity. Declaring one explicitly gives it normal ARG scope and cache behavior. Host proxy forwarding is controlled by `--http-proxy`.

### Timestamps and cache age

`SOURCE_DATE_EPOCH` accepts a supplied Unix timestamp or global ARG default; empty clears it. It controls image/history creation and reaches RUN only after stage redeclaration. `--source-date-epoch` overrides the resolved value; `--source-date-epoch=` clears the environment default. An empty environment value is unset, and negative Unix timestamps are accepted. An explicit numeric build argument overrides an authored `context` default.

| Epoch source | Value |
| --- | --- |
| `context` | Primary Git committer time; HTTP Last-Modified, otherwise newest regular-file archive mtime; unset for local context. |
| Named context | Same Git/HTTP rules; unset for local directory, image, or OCI layout. |
| Source-only stage name | Remote ADD metadata. Stage must use FROM scratch, ARGs, and exactly one HTTP(S)/Git ADD. |

`--timestamp` forces creation and new-layer file times, including older files and package snapshots. It conflicts with a nonempty source date epoch, including one from the environment. `--rewrite-timestamp` clamps newer layer/package file times when an epoch is set; without an epoch it does not clamp them. Timestamp policies enter cache identities.

`--cache-ttl DURATION` limits reads by wall-clock publication age, independently of image timestamps. Zero disables reads while permitting fresh writes.

## Build contexts and local files

A positional definition path uses its parent directory as context. With `--file FILE`, the context is the positional operand or the current directory. A relative FILE resolves from the working directory first; when absent there, Coopr looks inside the selected local, extracted archive, or remote context. Repeated files resolve independently against that shared context and append their global declarations and complete stage blocks in order.

Definition and primary context are independent inputs. Either can use stdin, but both cannot do so in one request. Contexts accept directories, local tar archives, stdin tar, Git checkouts, or HTTP(S) tar archives. `--file` also accepts an HTTP(S) definition. Definitions selected inside remote/archive contexts must remain inside them after symlink resolution; explicit local definitions may live outside the context.

Ignore selection is `--ignorefile`, otherwise the first context-root file: `.cooprignore`, `.containerignore`, `.dockerignore`. Docker/Podman patterns filter local COPY/ADD, RUN binds, and local component definitions; filtered inputs enter cache identity. Definition placement does not change ignore selection. Paths/symlinks cannot escape the selected context.

Local contexts exclude host `security.selinux` labels. Other xattrs, including `security.capability` and `user.*`, remain subject to runtime and exporter limits.

### Named contexts

Named contexts take precedence over image references and stage aliases in FROM sources, COPY/ADD `from=`, and RUN mount `from=`. Substituting a FROM's source keeps its instruction body. A context matching the FROM stage's own `as=` name replaces that stage and skips its body; numeric references still select stages. EXTEND and PACKAGE stage names are not replaced this way.

Only contexts needed by the selected graph materialize. Local named contexts use their own ignore files and freeze contents before execution. Context overrides supplied while packaging a component apply to package production and are not saved as invocation overrides. At invocation, named contexts and local inputs come from the caller.

For transport syntax, Git selectors, and credentials, see [named inputs](cli.md#named-inputs) and [remote inputs](definition.md#remote-inputs).

## Build request controls

Both build commands accept the following RUN and lifecycle settings. Defaults not given here come from the native configuration or leave the setting unspecified. See [CLI build controls](cli.md#build-controls) for inputs and outputs and [security](../guides/security.md) for host requirements.

| Group | Options and defaults |
| --- | --- |
| Scheduling | `--jobs` / `-j` limits total stage/platform work (1; 0 unlimited). `--skip-unused-stages=true` executes only the selected output’s dependencies. |
| Proxy and DNS | `--http-proxy=true` forwards host proxy variables. Repeatable `--dns`, `--dns-search`, `--dns-option` configure RUN resolution; `--dns none` disables DNS configuration. |
| Network | `--network` selects `default`, `private`, `none`, `host`, `ns:PATH`, `pasta[:OPTIONS]`, or a named network. Repeatable `--add-host HOST:IP` adds host entries. `--no-hosts` and `--no-hostname` preserve the image’s files. |
| Memory | `--memory` / `-m`, `--memory-swap` (memory plus swap; -1 unlimited swap), `--shm-size`. |
| CPU | `--cpu-period`, `--cpu-quota` (-1 unlimited), `--cpu-shares` / `-c`, `--cpuset-cpus`, `--cpuset-mems`. |
| Limits and groups | Repeatable `--ulimit NAME=SOFT:HARD`, `--group-add`. |
| Mounts and devices | Repeatable `--mount SPEC`, `--volume HOST:CONTAINER[:OPTIONS]` / `-v`, `--device`. CDI lookup uses global `--cdi-spec-dir`; hidden `--cdi-config-dir` overrides that list for a build. |
| Capabilities and security | Repeatable `--cap-add`, `--cap-drop`, `--security-opt` apply native security settings to all RUNs. |
| User namespaces | `--userns host\|private\|auto\|PATH`, `--userns-uid-map`, `--userns-gid-map` (`CONTAINER:HOST:SIZE`), `--userns-uid-map-user`, `--userns-gid-map-group`. |
| Other namespaces | `--cgroupns`, `--pid`, `--ipc`, `--uts` (`host`, `private`, or a namespace path); `--cgroup-parent`. |
| Runtime | `--isolation oci\|rootless\|chroot`, `--runtime`, repeatable `--runtime-flag`, `--hooks-dir`. Global `--module`, `--cgroup-manager`, `--network-config-dir` select native configuration. |
| Instruction layers | `--layers=true` caches instructions and preserves their layers; `BUILDAH_LAYERS` can change the default. See [layer rules](#layers-storage-and-copying). |
| Build containers | `--rm=true` removes intermediate containers after success; `--force-rm=true` also removes them after failure. To retain successful containers, disable both. |
| Stages and volumes | `--save-stages`, `--stage-labels`, `--compat-volumes`; details below. |

`--jobs` changes concurrency without changing dependency order or cache identity. Authored RUN network values override the build-wide default. Both build commands accept `--stdin` to pass command stdin to RUN; stdin cannot simultaneously supply the definition or context.

RUN resource, namespace, and runtime settings affect cache reuse; host volume/device contents are not measured. Memory and swap apply independently; explicit shared-memory zero differs from omission. Host permissions and cgroup delegation can reject resource requests.

Image `--env` prepends values to every stage; authored ENV wins. Bare names import present host values, `PREFIX*` imports matching names, and `*` imports all. Repeated context names and secret/SSH IDs use the last value.

Label/environment unsets, inheritance, and history omission apply to final image outputs, including cache hits. OCI annotations are dropped with Docker format. These output controls are listed in [CLI image output controls](cli.md#image-output-controls).

`--all-platforms` discovers common runnable Linux platforms across registry/native-store non-scratch bases and conflicts with explicit `--platform`. Discovery does not apply source-policy conversions; use explicit platforms for local transports or generated image sources. `--manifest NAME` serializes updates to a named local index, preserves other platforms, and replaces newly built instances.

### Outputs and failure recovery

Repeatable `--tag` assigns local names to one retained immutable result. With `--push`, those names select registry publication; the result remains stored by ID or digest. For later transfers, use `push` with an [image transport destination](cli.md#names-and-transfer-destinations). Components support registry and OCI archive transfers. Plain `push` destinations select a registry. `copy` is an alias for `tag` and only assigns local names.

`--metadata-file` records `containerimage.digest`, `containerimage.descriptor`, `coopr.platforms`, `coopr.references`, and per-destination `coopr.outputs`. A single-platform image also includes `containerimage.config.digest` when available; multi-platform image configuration digests appear in the per-platform entries.

Image `--iidfile` writes the algorithm-prefixed native image or manifest-list ID. `--iidfile-raw` (alias `--raw-iidfile`) writes the unprefixed image ID and requires one platform. Both files omit a trailing newline and suppress the final build ID line, including in logfiles. File-output parents are checked before execution.

Successful execution retains the result/checkpoints before transfers. Destinations commit independently; there is no transaction spanning engines or registries.

| Failure | Retained state and recovery |
| --- | --- |
| Transfer failure or cancellation | Reports the retained digest and completed destinations. Metadata records complete/failed/pending destinations and `coopr.outputError`. Check the destination before retrying: an interrupted transfer may have committed remotely. |
| Result-file finalization failure | Reports retained outputs. |
| Execution failure | Publishes neither newly staged portable-cache candidates nor final output names. A failed RUN preserves its exit status and produces one command diagnostic. |

Retry a transfer with `coopr push` or `coopr component push`. See [build output](cli.md#build-output) for progress, quiet and logfile behavior.

Registry resolution, caches, and publication share the request's [authentication and trust configuration](configuration.md#runtime-and-trust).

Signature policies apply to original image sources. Native image exports and index pushes verify each stored image before preparing private output. Continuing from that output does not require allowing directory or OCI-layout inputs in your policy. Caller-supplied layouts and directories remain subject to the configured policy.

## Resolution, execution, and caching

### Image selection

Image tags default to local cached selection. For image inputs, `--pull` accepts:

| Policy | Image selection |
| --- | --- |
| `missing` (default) | Use local content when available; pull a missing input. |
| `always` | Resolve from the source again. Bare `--pull` selects this policy. |
| `newer` | Compare selected digests; may reuse a local image on registry failure. |
| `never` | Require local content; fail missing image inputs before registry access. |

Digest references select exact objects and can reuse local copies offline. Reproducibility requires pinning every selected OCI reference, including nested ones. Partially imported registry indexes default to native Linux selection. Single-platform local tags select their stored platform; bare manifest digests select that exact manifest regardless of host.

Component calls check the component store by ordinary name before resolving a missing name from a registry. Bare component digests select local artifacts. Store errors fail the build instead of falling through to a registry. Components are not engine-loadable images; image `--pull` policy does not refresh a component already found locally. Use `coopr component pull` explicitly to refresh one.

FROM accepts plain image names, `docker://`, `containers-storage:`, `oci:`, `oci-archive:`, `docker-archive:`, `dir:`, `docker-daemon:`, and the native `atomic:` OpenShift transport. Filesystem transports read paths inside the selected build context, including through symlinks; they cannot read arbitrary host paths. Native transport selection preserves the selected image's configuration and layers. `docker-daemon:` imports Docker's exported compatibility image: the requested platform validates that image, rather than selecting an index child. Use a registry source for index platform selection, or Docker's platform-scoped save followed by `docker-archive:`; see [Docker imports](../guides/storage.md#transfer-to-docker-engine).

`--source-policy-file` loads Buildah's BuildKit-format image policy once for the build. ALLOW permits a matching source, DENY rejects it, and CONVERT substitutes its reference. Evaluation follows named image-context substitution and precedes image resolution. This policy is distinct from the native signature policy. Invalid policy files fail before execution.

For generated image sources, FROM `after` names one earlier stage by alias or numeric index. The dependency delays source selection and inherited-trigger planning until that producer finishes. Use it when a producer writes an OCI layout or archive into the writable primary context. Ordinary source order alone does not establish this dependency for parallel or unused-stage-skipping builds.

### RUN mounts

Authored mount children apply to their RUN. Repeatable `--mount` adds mounts to every executed RUN, including component and inherited RUNs, after the authored mounts. Mount `from` references participate in dependency planning and input identity; a self-dependency or cycle fails before execution.

A writable bind of the primary context uses one filtered, disposable copy for the build. Changes persist across RUNs, stages, and component calls, so later file operations and generated image sources see them. The original context is unchanged. Writes to image or named-context bind mounts use disposable per-RUN overlays and do not persist to subsequent RUNs.

For example, these instructions inside a FROM write a file into that disposable context, then copy it into the image:

```kdl
run "echo generated > /src/message.txt" {
    mount "bind" source="." target="/src" rw=#true
}
copy "message.txt" "/message.txt"
```

RUNs that write the primary context execute rather than reusing instruction results: an image snapshot cannot restore their context side effects. Later COPY/ADD and read-only context mounts measure the resulting files. Components using caller-context bind mounts do not reuse whole-component results.

Image VOLUME paths exist in final filesystem exports. With `--compat-volumes`, each RUN's changes under declared volumes are discarded at that RUN's boundary, even with `--layers=false`; COPY/ADD changes remain.

### Cache scopes

Reuse requires matching input state, arguments, platform, execution settings, and declared sources.

Instruction keys use the ordered OCI filesystem layers rather than the image's creation time. Execution configuration and captured root metadata still participate. Repeated component builds can reuse instructions without timestamp flags; cached results retain the current caller's history, labels, and annotations.

| Cache | Reuses | Limits |
| --- | --- | --- |
| Local instruction | RUN, COPY, ADD, and WORKDIR checkpoints across image and component phases. | Metadata-only changes need no filesystem checkpoint. |
| Portable instruction | RUN, COPY, ADD, and WORKDIR checkpoints from an OCI cache. | Requires identified inputs and execution settings; source resolution and request authorization still run. |
| Component state | The selected component output. | Requires a fixed invocation graph with fully identified inputs; see below. |
| Package result | Completed package snapshots. | All selected package results must be available; otherwise their producers execute. |
| Cache mount | Disposable tool data. | Names are scoped to their use unless explicitly supplied. Builds must work with empty mounts. |

COPY/ADD measure their selected inputs. Checksum-pinned HTTP ADD can hit without downloading again; Git ADD resolves the commit and submodules before checking the cache. A cache hit therefore does not guarantee an entirely offline build.

Whole-component reuse requires a resolved invocation graph. COPY/ADD and bind inputs must identify retained stages, packages, or selected images and named contexts. Dynamic bases, generated image inputs, nested component calls, and caller-context inputs still pass through planning and execution, where individual instructions can reuse their snapshots. This keeps transitive input resolution and context side effects visible.

Package-result caching supports networked and device-backed RUN, bind/cache/tmpfs/secret/SSH mounts, and checksum-pinned remote ADD. Volume compatibility, layer policy, and stage-label settings participate in package-result identity. Mutable remote ADD sources, nested components, dynamic bases, generated image inputs, and unplanned ONBUILD triggers must resolve before individual instructions reuse results instead of returning a whole-package hit. Writable context mounts execute to replay their side effects.

Effective device names after entitlement aliases participate in cache identity, including whether an optional CDI device is present. Required devices and authorization are checked before reuse. Live device contents, network responses, credentials/SSH-agent state, cache-mount contents, clock, randomness, and undeclared host inputs do not invalidate conventional cache results. A cache hit is not a reproducibility guarantee. Required reusable files belong in packages. Cache repositories are trusted inputs and may contain intermediate files absent from final images; protect them accordingly.

`--no-cache` bypasses instruction, component-state, and package-result reads, writes fresh results, and leaves cache mounts intact. `--cache-from` reads portable results; `--cache-to` writes them. Validated hits can seed another writable destination; read-only sources are never updated and write-only destinations are never queried. See the [caching guide](../guides/caching.md) for registry/local syntax and refresh commands.

### State identity and metadata limits

Portable instruction cache identity includes the ordered filesystem layers and execution configuration, excluding creation time and non-execution history metadata. Equal flattened filesystems with different layer chains need not share a key.

Component-result cache identity also includes the input image chain. Restoration retains the selected output's image graph, configuration and root metadata; identity ignores the root directory's modification time. Metadata that the exporter cannot preserve can prevent portable reuse.

The pinned Buildah backend can discard changes to root-directory mode, owner, or portable xattrs at layer commit. Coopr accepts such builds even when committed images cannot represent them. Package snapshots and cache checkpoints capture/restore these root attributes. Portable state snapshots cannot reuse outputs whose metadata the exporter cannot preserve, including subsecond file mtimes and nonportable xattrs.

### Layers, storage, and copying

OCI output preserves the base filesystem layers and configuration; native export may change their compression and manifest digest. Docker format selects schema 2 config/manifests and manifest lists. Component invocation preserves the selected output’s layers. Packages are invocation inputs, not appended consumer layers.

| Control | Layer boundary | Cache behavior |
| --- | --- | --- |
| Default `--layers=true` | One layer per filesystem instruction, including instructions inside components. Empty/configuration-only changes add none. | Eligible instructions reuse snapshots. |
| Authored `layer { ... }` | One net filesystem layer when the outer group closes; preceding layers remain. Nested groups are absorbed. | Instructions remain eligible for reuse within the group. |
| `--squash` (image build) | Combine the final image’s new layers, retaining its base layers. | Output compaction permits instruction reuse. |
| `--squash-all` (image build) | Combine the final image’s layers, including its base. | Output compaction permits instruction reuse. |
| `--layers=false` | Combine newly executed filesystem instructions into one layer; overrides authored group boundaries. | Rerun filesystem instructions instead of using instruction snapshots. |

`--squash` and `--squash-all` are mutually exclusive. An authored layer group requires one image lineage: an independently based replacement component output fails inside it, including with `--layers=false`. Outside an authored group, `--layers=false` permits replacement and adopts the selected output's base. See [layer groups](definition.md#layer-groups) for authored scope.

`--save-stages` retains completed intermediate stage images; instruction-cache snapshots follow their separate cache lifecycle. `--stage-labels` requires saved stages and adds `io.buildah.stage.name` and `io.buildah.stage.base` to stages with instructions. FROM-only stages reuse their base without adding labels.

`coopr tag` and its `copy` alias assign local names to the stored image or complete index. `coopr push` transfers a local image or index; complete locally built indexes transfer as complete indexes by default, and `--platform` selects one child. Destinations must support the selected image or index format. Docker’s classic store requires that selection; complete-index transfer requires an Engine store that supports indexes and passes Coopr’s capability, digest, and platform checks. Use `coopr pull docker-daemon:NAME` to import an Engine image. Engine conversion may change representation; `--format docker` avoids relying on Docker's OCI conversion.

Pruning follows [storage lifecycle rules](../guides/storage.md#inspect-and-maintain); external Podman access uses native storage locking.

### Output compression

Image builds accept `--compression-format gzip|zstd|zstd:chunked`, `--compression-level`, and `--force-compression`. A specified format defaults force-compression to true unless explicitly disabled. Otherwise native `containers.conf` defaults apply. Docker-format manifests require compatible layer compression.

These controls apply when exporting final image layers and portable instruction-cache images, including after cache hits and when the final stage is unchanged. They do not change instruction cache keys or force RUNs to execute. The native image store retains its normal filesystem/layer representation; filesystem and tar exports are not compressed image manifests.
