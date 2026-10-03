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
| `component` | Invoke an explicit context path (`./`, `../`, or `/`) or an OCI component reference, with string properties as arguments. |
| `run` | Execute a shell command or an exec argument list. |
| `copy`, `add` | Add context/stage/image files; ADD also supports remote inputs and archive extraction. |
| `arg`, `env` | Declare scoped build arguments or image environment. |
| `workdir`, `user` | Set working directory or user. |
| `cmd`, `entrypoint`, `shell` | Set execution defaults or shell. |
| `label`, `maintainer` | Set image metadata. |
| `expose`, `volume`, `stopsignal` | Set runtime configuration. |
| `healthcheck`, `onbuild` | Preserve healthcheck or inherited build-trigger configuration. |

Unknown instructions or unsupported options fail explicitly. Parser acceptance alone is not runtime support.

## Arguments

```kdl
arg "VERSION" "1"
from "docker.io/library/alpine:${VERSION}" as="base"
arg "channel" "stable"
env APP_CHANNEL="${channel}"
```

Global ARGs precede the first stage. Local ARGs apply from declaration onward and pass to a stage inheriting that stage with FROM. Independent extend roots get global and explicitly supplied component arguments, rather than caller-local arguments. Values are strings; unset declarations expand to empty text. Structural references must still be valid after expansion.

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

`healthcheck NONE` disables a healthcheck and takes no other options. Quoted `"NONE"` remains a shell command; `exec "NONE"` remains an executable argument. ONBUILD takes one ordinary instruction; nested ONBUILD, FROM, and MAINTAINER are rejected as inherited triggers. Later FROM executes inherited triggers in stored order using the child's context, then clears them.

For exact phase, resolution, metadata, timestamp, and cache behavior, see the [execution contract](execution.md).
