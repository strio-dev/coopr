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

Repeat `--file` to compose definitions. Coopr appends every file's instructions in command-line order, including FROM declarations, and uses one shared context. A file without a new stage declaration continues the preceding stage. Paths remain explicit; Coopr does not search for default definitions.

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

FROM also accepts native [image transports](../reference/execution.md#image-selection), including an OCI layout within the context. If an earlier stage generates that layout, add `after="producer"` to its consuming FROM, where `producer` is the earlier stage's name. This dependency keeps the producer reachable and delays image selection until it finishes, including with parallel builds. It does not inherit the producer's filesystem.

Writable bind mounts of the primary context share a disposable copy across RUNs, stages, and component calls. Generated files can feed later COPY/ADD or FROM inputs without modifying the source checkout. Writable mounts of named contexts and images remain disposable per RUN. Use `--mount SPEC` to apply a RUN mount across the build.

## Platforms and outputs

```sh
coopr build image.coopr --platform linux/amd64,linux/arm64 --tag app:multi
coopr copy app:multi oci-archive:app.oci.tar
coopr copy app:multi docker:app:amd64 --platform linux/amd64
```

Multiple platforms produce an OCI index, or a Docker manifest list with `--format docker`. Only Linux is supported. Foreign-architecture RUNs need host emulation; an assembled index does not prove the binaries execute.

Image format and destination are separate choices. `--format docker` changes the stored manifest/config format; `docker:` selects an Engine destination. See [security and limitations](security.md#supported-limitations) for runtime compatibility.

`--output type=local,dest=PATH` exports the final filesystem, and `--output type=tar,dest=PATH` exports a tar archive. Run `coopr build --help` for the full option list.

`--compression-format` selects gzip, zstd, or zstd:chunked for exported image layers and portable instruction caches. Docker-format output requires compatible compression. Compression changes apply even when instructions hit cache; the native image store keeps its usual filesystem representation.

## Refresh inputs

```sh
coopr build image.coopr --pull
coopr build image.coopr --pull=never
```

The default `missing` policy reuses locally selected images. `--pull` selects `always`; `never` requires local images. `newer` compares registry content and permits local reuse if the registry request fails. Component registry tags resolve on each build. Pin references by digest when selection must be immutable, including references inside components.
