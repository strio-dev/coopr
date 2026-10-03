# Build images

`coopr build FILE` builds the exact definition path you supply. There is no default filename or automatic `.coopr` suffix. To supply a context directory, archive, Git source, or stdin, select its definition with `--file FILE`. `--context` selects a different input tree for a definition supplied as the positional argument.

```sh
coopr build . --file build/release.coopr --tag app:dev
coopr build image.coopr --target runtime --build-arg VERSION=1.2
```

Local COPY/ADD sources stay inside the context after symlink resolution. Coopr selects the first available ignore file at the context root: `.cooprignore`, `.containerignore`, then `.dockerignore`. `--ignorefile PATH` overrides that selection. All three use Docker/Podman ignore patterns, including `!` negation. Coopr excludes its selected stores and temporary artifacts from the context.

## Stages and inputs

```kdl
from "docker.io/library/golang:1.26" as="build"
workdir "/src"
copy "." "."
run "go build -o /app ./cmd/app"

from "scratch" as="runtime"
copy "/app" "/app" from="build"
entrypoint {
    exec "/app"
}
```

This pattern assumes the application builds a self-contained executable; choose a runtime base or explicitly build statically when it needs libraries. The final stage is the default output. `--target` chooses a named stage, and unrelated stages are skipped by default.

Named inputs can be supplied separately:

```sh
coopr build image.coopr --build-context shared=../shared
```

Use `copy "file" "/file" from="shared"` to read that context. Image and OCI-layout contexts use `docker-image://REFERENCE` and `oci-layout://PATH:TAG`; Git and HTTP tar inputs are also supported. Context names can replace named stages. Numeric stage references still select stages.

## Platforms and outputs

```sh
coopr build image.coopr --platform linux/amd64,linux/arm64 --tag app:multi
coopr copy app:multi oci-archive:app.oci.tar
coopr copy app:multi docker:app:amd64 --platform linux/amd64
```

Several platforms produce an OCI index, or a Docker manifest list with `--format docker`. Only Linux is supported. Foreign-architecture RUNs require host emulation; copying files or assembling an index does not prove those binaries execute.

Image format and destination are separate choices. `--format docker` changes the stored manifest/config format; `docker:` selects an Engine destination. Healthchecks and ONBUILD extensions are retained in both formats, but runtime handling varies.

`--output type=local,dest=PATH` exports the final filesystem, and `--output type=tar,dest=PATH` exports a tar archive. Consult `coopr build --help` for output controls, resource limits, lifecycle flags, and metadata options.

## Refresh inputs

Image pulls default to `--pull-policy missing`, reusing a local selection. `--pull` selects `always`; `--pull-policy never` requires local inputs. `newer` compares the registry selection and permits local fallback on registry failure. Component registry tags resolve on each build. Use digest pins for immutable selection, including references nested in a component.
