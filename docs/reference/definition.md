# Definition syntax

Write `.coopr` files in [KDL v2](https://kdl.dev/spec/). `from`, `extend`, and `package` each own their instructions inside `{ ... }`. Instructions execute in source order within that block. A definition with `extend` describes a component; one without it describes an image.

## Instructions

| Instruction | Meaning |
| --- | --- |
| `from`, `extend`, `package` | Start [image, caller, or package stages](#stage-blocks). |
| `layer` | Group instructions into one [net filesystem layer](#layer-groups). |
| `component` | Invoke an explicit context path (`./`, `../`, or `/`) or an OCI component reference, with `arg` children binding invocation arguments. Append `#NAME` to a local path to [select an output](../guides/components.md#select-outputs-and-arguments). |
| `run` | Execute [shell text or literal arguments](#command-forms). |
| `copy`, `add` | [Copy files](#files-and-mounts); ADD also fetches remote inputs and extracts archives. |
| `arg`, `env` | Declare [build arguments](#arguments) or image environment. |
| `workdir`, `user` | Set working directory or user. |
| `cmd`, `entrypoint`, `shell` | Set execution defaults or shell. |
| `label`, `maintainer` | Set image metadata. |
| `expose`, `volume`, `stopsignal` | Set runtime configuration. |
| `healthcheck`, `onbuild` | Set [healthchecks and inherited build triggers](#healthchecks-and-triggers). |

## Stage blocks

A definition contains top-level global `arg` declarations and stage blocks. Put every stage instruction inside its stage’s braces, including `arg`, `component`, and `layer`. An `extend` may also contain FROM blocks. Their dependencies and consumers determine when they run, just as for top-level FROM stages. Other stage nesting is forbidden, including stages inside `layer`. Empty stages still need braces, such as `extend {}`.

```kdl
arg "base" "docker.io/redhat/ubi9:latest"

from "${base}" as="runtime" {
    run "echo hello"
    cmd {
        exec "echo" "hello"
    }
}
```

Images default to the last top-level FROM; components default to the last top-level EXTEND, even when FROM or PACKAGE stages follow it. `--target NAME`, or `#NAME` on a local component reference, selects another named output. Component targets must be top-level EXTEND stages. FROM stages can supply packaging or invocation dependencies; PACKAGE stages capture packaged inputs. Trailing stages outside the selected output’s dependencies are skipped.

Inside `extend`, direct instructions modify the caller and nested FROM blocks own separate stages. The last non-ARG child determines the output: a nested FROM returns its image; a direct instruction returns the modified caller. ARG declarations alone do not switch the output. Stage aliases must be unique throughout the definition, including nested stages. A local stage must be completed before the FROM, COPY/ADD, or RUN mount that consumes its result. Self-references and references to later stages are errors. Inside EXTEND, a nested FROM becomes available to subsequent instructions and nested stages.

Stage names start with a letter and may contain letters, digits, `_`, `.`, and `-`. Names match case-insensitively; `scratch` is reserved. Stage sources also accept zero-based numeric indices, counting nested FROM stages.

FROM accepts `as`, `platform`, and `after`. `after="NAME"` names one earlier stage (or its numeric index) that must finish before the external image is resolved. Use it for an image layout or archive generated through a writable primary-context mount. It adds a dependency without inheriting that stage's state. See [image selection](execution.md#image-selection) for accepted transports and context confinement.

The examples below show instruction fragments unless they include a stage block. Put fragments inside `from`, `extend`, or `package` as appropriate.

## Values and forms

Instructions are lowercase KDL nodes. Arguments follow the instruction name; options use `name=value`. Child instructions and options go inside `{ ... }`.

| Value | Accepted forms |
| --- | --- |
| Text | `"hello"`, `"/usr/local/bin/app"`. |
| Boolean options | Native `#true` / `#false`, or quoted `"true"` / `"false"`. |
| Numbers | Native numbers, such as `retries=3` or `uid=1000`. |
| Unix permissions | Native octal `chmod=0o755`, or quoted octal digits `chmod="0755"`. |
| Parameter expressions | Quoted strings, such as `chmod="${permissions}"`. |

Bare `true` and `false` are invalid KDL v2 values. Null, type annotations, and nonfinite numbers are unsupported. A leading zero does not make a KDL number octal: use `0o755` for a native octal literal.

```kdl
copy "app" "/usr/local/bin/app" chmod=0o755 link=#true
```

### Command forms

A command string runs through the configured shell. An `exec` child passes an executable and literal arguments; shell variables, pipes, and redirects need shell form.

```kdl
run "echo $HOME"

run {
    exec "echo" "hello"
}

cmd {
    exec "/usr/local/bin/app" "--serve"
}
```

A `script` child is the block form of a shell command. RUN accepts multiple `script` and `exec` children, executed in order:

```kdl
run {
    script "echo \"$HOME\" > /home-path"
    exec "cat" "/home-path"
}
```

The commands share the RUN's mounts and devices and stop at the first failure. The whole RUN caches as one instruction and produces one filesystem layer when files change, with default layering. Each command starts with the configured environment and working directory. Filesystem changes carry between commands; `cd` and `export` affect only the shell running that script. Put commands needing shared shell state in one script.

CMD, ENTRYPOINT and HEALTHCHECK accept one command string or exactly one `script` or `exec` child. They configure a runtime command instead of executing it during the build. Empty CMD or ENTRYPOINT, including an empty `exec` child, clears that image setting. ONBUILD RUN accepts only one command.

Do not combine a command string with command children. RUN mounts and devices are siblings of `script` and `exec`:

```kdl
run {
    mount "bind" source="." target="/src"
    exec "ls" "/src"
}
```

SHELL takes the shell executable and its arguments directly:

```kdl
shell "/bin/bash" "-c"
run "echo hello"
```

It configures shell-form RUN, CMD and ENTRYPOINT, including their `script` children. HEALTHCHECK `script` uses the CMD-SHELL healthcheck form. SHELL arguments and MAINTAINER text stay literal; build arguments and environment variables are not substituted into them.

### Multiline commands

Use ordinary triple-quoted KDL strings for multiline shell scripts:

```kdl
run """
    mkdir -p /app &&
    printf 'hello\\n' > /app/message
    """
```

The opening delimiter is followed by a newline; the closing delimiter goes on its own line. KDL removes the closing delimiter's indentation. Normal string escapes still apply: write `\\` to pass a literal backslash to the shell, as in the `printf` above.

A positional multiline RUN starting with a `#!` interpreter line executes with that interpreter. An explicit `script` child always uses the configured shell, even if its text starts with `#!`. COPY/ADD can read source files or create inline files with named `file` children (see below).

## Layer groups

```kdl
from "docker.io/redhat/ubi9:latest" {
    layer {
        run "dnf install -y jq"
        run "dnf clean all"
    }
}
```

`layer` accepts no arguments or properties. Its children are ordinary stage instructions, including component calls and nested groups, executed in source order. ARG declarations and configuration changes have the same scope as instructions outside the block. FROM, EXTEND and PACKAGE declarations are forbidden inside it.

Closing the outermost group keeps only the net filesystem change above its starting image. Inner groups are absorbed by the outer one. Empty and configuration-only groups add no filesystem layer. Groups preserve preceding layers and resulting image configuration.

A group requires one image lineage. A component output with an independent FROM base is rejected inside the group, even if its filesystem happens to match the starting image. Use that component outside the group. `--layers=false` takes precedence; see [layer behavior](execution.md#layers-storage-and-copying). The lineage rule still applies.

## Arguments

```kdl
arg "version" "9"

from "docker.io/redhat/ubi${version}:latest" as="base" {
    arg "version"
    arg "channel" "stable"
    env APP_CHANNEL="${channel}"
}
```

Top-level global ARGs support structural expansion. Redeclare a global inside a stage with `arg "NAME"` to consume its value and expose it to RUN; a stage declaration with a value supplies a stage default. Stage ARGs apply from declaration and pass through FROM inheritance. Independent EXTEND roots get global and explicitly supplied component arguments. Values are strings; unset references expand to empty text. Structural references must remain valid after expansion. Package-producing arguments are fixed at component build; see [argument scope](execution.md#arguments-and-normalization).

Pass arguments to a component inside its call block:

```kdl
component "./components/shared.coopr#configured" {
    arg "channel" "api"
    arg "target" "ordinary-value"
}
```

Only `arg` children are accepted, each with a name and an explicit value. Values can use caller argument or environment expansion. Bindings stay private to the invocation; they do not declare caller ARGs. `target` is an ordinary argument name, separate from the local path’s output selector. Calls without bindings need no block.

Boolean options can use arguments. Quote permission defaults so their octal digits survive substitution:

```kdl
from "docker.io/redhat/ubi9:latest" {
    arg "linked" #true
    arg "permissions" "0755"
    copy "app" "/usr/local/bin/app" link="${linked}" chmod="${permissions}"
}
```

Numeric ARG values are normalized as decimal text. Mount types and option values support `$name`, `${name}`, and defaults such as `${name:-cache}`.

## Files and mounts

```kdl
copy "a" "/a" { exclude "*.md" "*.tmp" }
run "make" {
    mount "cache" target="/root/.cache"
    mount "secret" id="token" target="/run/secrets/token" required=#true
}
```

COPY/ADD accept repeated `file "name" "content"` children. The last positional argument is the destination; ordinary source paths may precede it:

```kdl
copy "existing.txt" "/etc/app/" chmod=0o640 {
    file "greeting.txt" "Hello ${name}\n"
    file "literal.txt" #"${name} is literal"#
    file "empty.txt" ""
}
```

Filenames are literal local paths, including any `$` characters; they cannot be empty, absolute, `.` or escape their temporary source directory. Content uses the exact decoded KDL bytes, including empty bodies and final newlines. Ordinary strings expand ARG/ENV references in the current stage or component scope using native heredoc word expansion: quotes and backslashes remain data; single quotes and escaped dollars suppress expansion. Raw strings (`#"..."#` and `#"""..."""#`) disable that expansion. Both ordinary and raw KDL multiline forms retain their decoded multiline value; KDL removes the opening and closing boundary newlines. Filenames reject NUL. COPY/ADD content can include NUL bytes, written as `\u{0}` in an ordinary KDL string.

Inline files run as one native COPY/ADD batch before ordinary sources. Repeated filenames retain their order, and ordinary same-name sources overwrite inline files. Ignore files and `exclude` patterns filter ordinary sources; inline content bypasses them. Ownership, mode, link, destination and source-path handling use the existing native COPY/ADD operations. Inline data and expansion results participate in cache identity. Package-time argument references in expanding bodies become fixed package inputs; EXTEND bodies expand at invocation.

Child options retain their order. COPY reads context files, earlier stages, packages, named contexts, images, or OCI artifact files. ADD also fetches HTTP/Git sources and extracts archives; COPY rejects remote URLs before connecting. Files cannot escape the selected context through paths or symlinks. See [security](../guides/security.md) for credentials and entitlements.

### Copy OCI artifact files

Use the same `from` property for an OCI artifact published to a registry:

```kdl
from "docker.io/redhat/ubi9:latest" {
    copy "settings.conf" "/etc/app/settings.conf" from="registry.example.com/app/settings:1"
}
```

Artifact inputs can reference a single manifest or a platform index. An index selects a child manifest for the stage's platform using native image-list matching. Tags and index digests both work; cached platforms can be reused with `--pull=never`.

The selected manifest exposes its raw blobs as files, following `podman artifact extract`: a blob's `org.opencontainers.image.title` supplies its filename, or an untitled blob uses `sha256-<digest>`. Titles must be filenames without directory separators. Blob files use mode `0600`, matching Podman's artifact tar export; `chmod` overrides it. COPY retains archive files as files; ADD can extract a selected archive using its usual behavior. ORAS directory-unpacking annotations do not change this behavior.

Artifact inputs use Podman's artifact store beside the image store, at `graphroot/artifacts`, with the build's registry credentials, TLS and signature policy. Native local images take precedence over same-named artifacts. The build's pull policy also applies to artifacts, so a previously fetched artifact can be used with `--pull=never`. Artifacts supply files, not a runnable base image or inherited image configuration.

Artifact inputs are selected by immutable digest and verified before use. Concurrent removal or replacement in native artifact storage can make an active build fail.

## Healthchecks and triggers

```kdl
healthcheck interval="30s" timeout="3s" retries=3 {
    exec "/bin/check" "--ready"
}
onbuild { copy "generated" "/generated" }
```

Timing properties are `interval`, `timeout`, `start-period`, and `start-interval`. Supplied durations must be positive, including values below a millisecond; `retries` must be an integer from 1 through 2147483647. Omit an option to use the native default. `healthcheck NONE` disables a healthcheck and takes no other options. Quoted `"NONE"` remains a shell command; `exec "NONE"` remains an executable argument.

ONBUILD wraps one supported image instruction. Stage declarations, `maintainer`, `component`, `layer`, and nested `onbuild` are not supported inside it. Existing external MAINTAINER triggers can be imported and retain their literal text. A child FROM executes inherited triggers before its authored instructions, using the child's context, then clears them. Local stage sources must be available before that child FROM.

Inline COPY/ADD inside ONBUILD must round-trip through native Dockerfile heredocs. Nonempty content must end in LF and contain no complete line equal to its filename (the native delimiter). Embedded CR and NUL bytes are supported; CRLF is rejected because native parsing strips the CR at line endings. Empty bodies are rejected because native Buildah creates one LF byte. Filenames and repeated-name payloads must survive both native and Coopr source parsing exactly; unrepresentable names (including spaces) or conflicting duplicate payloads are rejected. Ordinary source paths and the destination must also survive the native shell-form header; whitespace-containing paths cannot be mixed with heredocs and are rejected. Coopr never appends bytes, renames files, or splits instructions to encode these cases. Ordinary bodies expand in the child stage; raw bodies remain literal.

ONBUILD stores Dockerfile trigger text using Buildah’s native instruction and mount options. RUN `network`, COPY/ADD boolean flags, and mount values can use child-stage arguments, including typed mount options such as `required` and `mode`. Healthcheck timing/retries and device `required` must remain literal.

Mount flags represented without values, including `readonly`/`ro`/`rw`, `nosuid`, `U`, and `Z`, require literal booleans in ONBUILD. Readonly booleans select native read-only or writable access; other toggles emit their flag when true and omit it when false. Cache `relabel` must be literal `private` or `shared`. Secret and SSH mounts cannot specify readonly access flags in ONBUILD.

A parameterized mount type can use `source`/`src` for bind/cache, read-only access for bind/cache/tmpfs, and `mode` for cache/secret/SSH. The child’s resolved type validates the native fields. Conversions that change the native field identity require a literal type: secret `source` becomes `id`, tmpfs `mode` becomes `tmpfs-mode`, and cache `relabel` becomes `Z`/`z`. A dynamic `mode` field is not silently rewritten when the child selects tmpfs.

For example, these mount values resolve in the child stage:

```kdl
onbuild {
    run "cat /run/token" {
        mount "secret" id="token" target="/run/token" required="${required:-true}" mode="${mode:-0400}"
    }
}
```

## Instruction options

The fixed properties for stage and execution instructions are:

| Instruction | Properties / children |
| --- | --- |
| `from` | `as`, `platform`, `after`; required stage body. |
| `extend` | `as`; required stage body. |
| `package` | Required `as` name and stage body. |
| `run` | `network`; repeatable `script`, `exec`, `mount`, and `device` children. |
| `copy` | `from`, `chown`, `chmod`, `link`, `parents`; repeatable `exclude` children listing patterns and named `file` children. |
| `add` | COPY properties plus `checksum`, `keep-git-dir`, `unpack`; `exclude` and named `file` children. |
| `healthcheck` | `interval`, `timeout`, `start-period`, `start-interval`, `retries`; one `script` or `exec` child instead of positional shell text. |
| `device` | One or more CDI selectors and optional `required` boolean; child of RUN. |
| `env`, `label` | User-defined key/value properties. |
| `component` | Component reference argument; optional `arg "NAME" "VALUE"` children. No properties. |

Other stage instructions accept no properties. CMD and ENTRYPOINT accept one `script` or `exec` child instead of positional shell text. `script`, `exec`, `file`, `exclude`, `mount`, and `device` are nested forms, not independent stage instructions.

ENV and LABEL accept key/value properties or one name/value pair. Do not mix the two forms in one instruction:

```kdl
env APP_CHANNEL="stable" APP_LOG_LEVEL="info"
env "APP_CHANNEL" "stable"
label "org.opencontainers.image.title" "hello"
```

COPY/ADD options have these effects:

| Property | Effect |
| --- | --- |
| `from` | Read the selected stage, package, named context, or image. |
| `chown` | Set destination ownership as `USER[:GROUP]` or numeric IDs. Named identities resolve in the destination image. |
| `chmod` | Set Unix permission bits; accepts octal forms described above. |
| `link` | Create an independent layer, committed before the next filesystem instruction. |
| `parents` | Retain source parent directories; `/./` marks where retained paths begin. |
| `checksum` (ADD) | Verify one remote source: `sha256:HEX` for HTTP, a commit prefix for Git. |
| `keep-git-dir` (ADD) | Retain Git metadata instead of removing it. |
| `unpack` (ADD) | Enable or disable archive extraction. |

Boolean properties accept the forms in [values and forms](#values-and-forms).

Healthcheck and ONBUILD configuration extensions survive OCI and Docker outputs, though receiving runtimes may ignore them. Imported Docker ONBUILD heredocs retain their Dockerfile semantics, including executable/empty RUN scripts and COPY/ADD filenames. Authored inline COPY/ADD uses the `file` children described above.

### RUN mount options

Mount types and values may use arguments and are validated after expansion. Mount children retain source order.

| Type | Canonical properties |
| --- | --- |
| `bind` | `from`, `source`, `target`, `readonly`, `bind-nonrecursive`, `no-dereference`, `consistency`, plus native bind options below. |
| `cache` | `from`, `id`, `source`, `target`, `readonly`, `sharing`, `mode`, `uid`, `gid`, plus native bind options below. |
| `tmpfs` | `target`, `readonly`, `size`, `mode`, `nosuid`, `nodev`, `noexec`, `tmpcopyup`. |
| `secret` | `id`, `source`, `target`, `required`, `mode`, `uid`, `gid`, `env`, `readonly`. |
| `ssh` | `id`, `target`, `required`, `mode`, `uid`, `gid`, `readonly`. |

`sharing` supports `shared` or `locked`. The bundled Buildah backend rejects `private`; use distinct cache IDs for separate caches. Secret `source`/`src` names a supplied secret ID; it does not read a host path directly. `size` sets the tmpfs capacity; `mode`, `uid`, and `gid` set mount-specific permissions and ownership. Secret `env` exposes the secret as an environment variable.

Bind/cache native options are `nosuid`, `nodev`, `noexec`, `u`, `shared`, `rshared`, `private`, `rprivate`, `slave`, `rslave`, `bind-propagation`, and `relabel`. Propagation accepts those six propagation modes; relabel accepts `private` or `shared`. Host ownership changes, relabeling, and mount behavior require the selected runtime’s permissions.

Aliases are `src` for `source`, `dst`/`destination` for `target`, `ro` for `readonly`, `rw`/`readwrite` for writable access, `tmpfs-size`/`tmpfs-mode` for tmpfs `size`/`mode`, `U` for `u`, and `z`/`Z` for shared/private relabeling. An alias is accepted only where its canonical property is supported.

Build-wide `--mount` specifications append after authored mounts. See [RUN mounts](execution.md#run-mounts) for source identity and writable-context lifetime.

Both build commands accept `--secret id=NAME,src=PATH`, `--secret id=NAME,env=VARIABLE`, and `--ssh ID[=PATH]`. Credential bytes are read when needed and do not enter definitions or cache keys. Changed credentials require `--no-cache` or another measured input change when they must change the output.

## Remote inputs

| Source | Credentials |
| --- | --- |
| HTTP file ADD | `HTTP_AUTH_HEADER_<host>` or `HTTP_AUTH_TOKEN_<host>` secrets, selected separately for each redirect host. |
| Private HTTPS Git | Host-scoped `GIT_AUTH_HEADER.<host>` or `GIT_AUTH_TOKEN.<host>`. Authenticated redirects fail; submodules outside the selected parent-repository scope receive no parent authorization header. |
| SSH/SCP Git | `--ssh default` plus `GIT_KNOWN_HOSTS.<host>` or `GIT_KNOWN_HOSTS`, with strict host-key verification. |

## Validation

Unknown instructions and option names fail during definition validation, including in unused stages. Parameterized values are checked after expansion in the selected stage. See the [execution reference](execution.md) for stage, input, and runtime rules.

Errors identify the instruction and nested child where applicable. Source coordinates use byte columns; line numbers are accurate for LF and CRLF files. The parser currently misreports line numbers for CR-only and Unicode line separators.
