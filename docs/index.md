# Build images. Reuse the parts.

<div class="coopr-intro" markdown>
Coopr builds Linux container images from ordered `.coopr` definitions.
Package common build steps and files as OCI components, then apply them to other images.
</div>

[Get started](getting-started/index.md){ .md-button .md-button--primary }
[Build your first image](tutorials/first-image.md){ .md-button }
{ .coopr-actions }

## Install Coopr

```sh
curl -fsSL https://coopr.strio.dev/install.sh | sh
coopr --version
```

Installs a Linux release to `/usr/local/bin`, using sudo for non-root installs. See [getting started](getting-started/index.md) for requirements and installation options.

<div class="coopr-paths" markdown>
<div class="coopr-path" markdown>
## Start with an image

Write a definition with familiar `from`, `run`, and `copy` steps. Build and tag the image locally.

[First-image tutorial](tutorials/first-image.md)
</div>
<div class="coopr-path" markdown>
## Share a component

Package reusable steps and their files together. Components transform the caller's image rather than becoming another base image.

[Two-component tutorial](tutorials/reusable-components.md)
</div>
</div>

## One ordered definition

Put `index.html` beside `image.coopr` to build a UBI9 image serving your page with NGINX:

```kdl
from "docker.io/redhat/ubi9:latest"
run "dnf install -y nginx && dnf clean all"
copy "index.html" "/usr/share/nginx/html/index.html"
expose "80/tcp"
cmd {
    exec "nginx" "-g" "daemon off;"
}
```

```sh
coopr build image.coopr -t hello:latest
```

Definitions use KDL. Instructions run in order within a stage; named stages connect through `from` and `copy`. See the [definition reference](reference/definition.md) for syntax and options.

## Find your next step

- [Build guides](guides/builds.md): contexts, targets, platforms, and output formats.
- [Component guide](guides/components.md): package artifacts and reuse transformations.
- [Storage](guides/storage.md): local images, components, and registry transfers.
- [Security](guides/security.md): rootless requirements, mounts, secrets, and nested containers.
- [Architecture](concepts/architecture.md): how resolution, planning, execution, and storage fit together.
