# Build images

Choose a definition file and a tag:

```sh
coopr build image.coopr --tag app:dev
```

Coopr reads KDL v2 from the exact path supplied; it does not choose a default filename or add `.coopr`. A positional definition uses its directory as the context. With `--file FILE`, the context defaults to the current directory. Supply a positional context to choose another:

```sh
coopr build . --file build/release.coopr --tag app:dev
coopr build image.coopr --target runtime --build-arg VERSION=1.2
```

Local COPY/ADD sources stay inside the context, including through symlinks. Ignore-file precedence is `.cooprignore`, `.containerignore`, then `.dockerignore` at the context root; `--ignorefile PATH` overrides it. Docker/Podman patterns and `!` negation apply. Selected stores and temporary artifacts are excluded.

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

The scratch example requires a self-contained executable; otherwise choose a runtime base. The final stage is the default output. `--target` selects a named stage; unrelated stages are skipped by default.

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

Multiple platforms produce an OCI index, or a Docker manifest list with `--format docker`. Only Linux is supported. Foreign-architecture RUNs need host emulation; an assembled index does not prove the binaries execute.

Image format and destination are separate choices. `--format docker` changes the stored manifest/config format; `docker:` selects an Engine destination. See [security and limitations](security.md#supported-limitations) for runtime compatibility.

`--output type=local,dest=PATH` exports the final filesystem, and `--output type=tar,dest=PATH` exports a tar archive. Run `coopr build --help` for the full option list.

## Refresh inputs

```sh
coopr build image.coopr --pull
coopr build image.coopr --pull=never
```

The default `missing` policy reuses locally selected images. `--pull` selects `always`; `never` requires local images. `newer` compares registry content and permits local reuse if the registry request fails. Component registry tags resolve on each build. Pin references by digest when selection must be immutable, including references inside components.
