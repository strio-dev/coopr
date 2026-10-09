# Definition syntax

Definitions use KDL v2 in `.coopr` files. Instructions run in source order within a stage. Any definition with `extend` is a component; definitions without it build images.

## Values and forms

Use quoted strings for text and Unix permissions, quoted `"true"`/`"false"` for boolean options, and native numbers for numeric options. Bare `true` and `false` are not valid KDL v2 values:

```kdl
copy "app" "/app" chmod="0755" link="true"
run "echo $HOME"
run {
    exec "/bin/sh" "-c" "echo hello"
}
cmd {
    exec "/app" "--serve"
}
cmd "echo hello"
```

RUN, CMD, ENTRYPOINT and HEALTHCHECK take one shell-command string or an `exec` child containing the executable and its arguments. An `exec` child is valid only under those instructions. Do not combine it with a shell-command string. RUN mounts and devices are siblings of `exec`; healthcheck timing stays on the parent. Empty CMD or ENTRYPOINT, including an empty `exec` child, clears that image setting.

Ordinary triple-quoted KDL strings hold multiline shell scripts:

```kdl
run """
    mkdir -p /app &&
    printf 'hello\\n' > /app/message
    """
```

KDL removes the closing delimiter's indentation. Normal string escapes still apply: write `\\` to pass a literal backslash to the shell, as in the `printf` above. Static content belongs in context files; authored COPY/ADD have no special inline-content syntax.

A multiline RUN starting with a `#!` interpreter line executes as a script with that interpreter, matching executable Containerfile heredocs.

## Instructions

| Instruction | Meaning |
| --- | --- |
| `from`, `extend`, `package` | Start an image, consuming, or package stage. |
| `layer` | Group ordered instructions into one net filesystem layer. |
| `component` | Invoke an explicit context path (`./`, `../`, or `/`) or an OCI component reference, with string properties as arguments. |
| `run` | Execute a shell command or an exec argument list. |
| `copy`, `add` | Add context/stage/image files; ADD also supports remote inputs and archive extraction. |
| `arg`, `env` | Declare scoped build arguments or image environment. |
| `workdir`, `user` | Set working directory or user. |
| `cmd`, `entrypoint`, `shell` | Set execution defaults or shell. |
| `label`, `maintainer` | Set image metadata. |
| `expose`, `volume`, `stopsignal` | Set runtime configuration. |
| `healthcheck`, `onbuild` | Preserve healthcheck or inherited build-trigger configuration. |

Unknown instructions or unsupported options fail explicitly. See the [execution reference](execution.md) for stage, input, and runtime rules.

FROM accepts `as`, `platform`, and `after`. `after="NAME"` names one earlier stage (or its numeric index) that must finish before the external image is resolved. Use it for an image layout or archive generated through a writable primary-context mount. It adds a dependency without inheriting that stage's state. See [image selection](execution.md#image-selection) for accepted transports and context confinement.

## Layer groups

```kdl
from "docker.io/redhat/ubi9:latest"
layer {
    run "dnf install -y jq"
    run "dnf clean all"
}
```

`layer` accepts no arguments or properties. Its children are ordinary stage instructions, including component calls and nested groups, executed in source order. ARG declarations and configuration changes have the same scope as instructions outside the block. FROM, EXTEND and PACKAGE declarations are forbidden inside it.

Closing the outermost group keeps only the net filesystem change above its starting image. Inner groups are absorbed by the outer one. Empty and configuration-only groups add no filesystem layer. Groups preserve preceding layers and resulting image configuration.

A group requires one image lineage. A component output with an independent FROM base is rejected inside the group, even if its filesystem happens to match the starting image. Use that component outside the group. `--layers=false` takes precedence and retains the existing whole-build no-layer behavior; the lineage rule still applies.

## Arguments

```kdl
arg "VERSION" "1"
from "docker.io/library/alpine:${VERSION}" as="base"
arg "channel" "stable"
env APP_CHANNEL="${channel}"
```

Global ARGs precede the first stage and support structural expansion; redeclare them in a stage for RUN exposure. Local ARGs apply from declaration and pass through FROM inheritance. Independent EXTEND roots get global and explicitly supplied component arguments. Values are strings; unset references expand to empty text. Structural references must remain valid after expansion. Package-producing arguments are fixed at component build; see [argument scope](execution.md#arguments-and-normalization).

## Files and mounts

```kdl
copy "a" "/a" { exclude "*.md" "*.tmp" }
run "make" {
    mount "cache" target="/root/.cache"
    mount "secret" id="token" target="/run/secrets/token" required="true"
}
```

Child options retain their order. COPY accepts local context and declared stage/image sources; ADD owns HTTP/Git fetch behavior. Files cannot escape the selected context through paths or symlinks. See [security](../guides/security.md) for credentials and entitlements.

## Healthchecks and triggers

```kdl
healthcheck interval="30s" timeout="3s" retries=3 {
    exec "/bin/check" "--ready"
}
onbuild { copy "generated" "/generated" }
```

Timing properties are `interval`, `timeout`, `start-period`, and `start-interval`; `retries` is a nonnegative integer. `healthcheck NONE` disables a healthcheck and takes no other options. Quoted `"NONE"` remains a shell command; `exec "NONE"` remains an executable argument. ONBUILD takes one ordinary instruction; nested ONBUILD, FROM, and MAINTAINER are rejected as inherited triggers. Later FROM executes inherited triggers in stored order using the child's context, then clears them.

EXTEND compatibility fields (`distro`, `distro-version`, `package-manager`, `architecture`) accept a string property or one child listing allowed strings. See [compatibility requirements](execution.md#compatibility-requirements) for matching rules.

## Instruction options

COPY accepts local or declared stage/image inputs; HTTP/Git sources fail before connection. ADD supports remote sources, archive extraction via `unpack="true"|"false"`, and Git metadata retention via `keep-git-dir="true"`. COPY/ADD `link="true"` creates an independent layer, committed before the next filesystem instruction so later instructions see it.

Healthcheck and ONBUILD configuration extensions survive OCI and Docker outputs, though receiving runtimes may ignore them. Imported Docker ONBUILD heredocs retain their Dockerfile semantics, including executable/empty RUN scripts and COPY/ADD filenames. This does not add authored inline COPY/ADD syntax.

RUN supports bind, cache, tmpfs, secret, and SSH mounts. Mount types/properties may use arguments and are validated after resolution. Mount children retain source order. Secret/SSH mounts accept `id`, `target`, `required`, `uid`, `gid`, and `mode`; secrets also accept `env`.

Bind mounts support `source`/`src`, `target`/`dst`/`destination`, `from`, and `rw`/`readwrite` or `ro`/`readonly`. Native ownership, relabeling, propagation, recursion, and `nosuid`/`nodev`/`noexec` options remain subject to the selected runtime's permissions. Build-wide `--mount` specifications use the same mount handling and append after authored mounts. See [RUN mounts](execution.md#run-mounts) for writable-context lifetime.

Both build commands accept `--secret id=NAME,src=PATH`, `--secret id=NAME,env=VARIABLE`, and `--ssh ID[=PATH]`. Credential bytes are read when needed and do not enter definitions or cache keys. Changed credentials require `--no-cache` or another measured input change when they must change the output.

## Remote inputs

| Source | Credentials |
| --- | --- |
| HTTP file ADD | `HTTP_AUTH_HEADER_<host>` or `HTTP_AUTH_TOKEN_<host>` secrets, selected separately for each redirect host. |
| Private HTTPS Git | Host-scoped `GIT_AUTH_HEADER.<host>` or `GIT_AUTH_TOKEN.<host>`. Authenticated redirects fail; submodules outside the selected parent-repository scope receive no parent authorization header. |
| SSH/SCP Git | `--ssh default` plus `GIT_KNOWN_HOSTS.<host>` or `GIT_KNOWN_HOSTS`, with strict host-key verification. |
