# Build your first image

Build a Red Hat UBI9 image that serves your own web page with NGINX.

Run these commands from the repository root after [building the CLI](../getting-started/index.md). Enter `nix develop path:.` to use Coopr and Podman. The first build needs access to Docker Hub and UBI's public package repositories; no Red Hat subscription is needed.

## Prepare the context

The example directory contains two files:

```text
examples/first-image/
  image.coopr
  index.html
```

`image.coopr` starts from UBI9, installs NGINX, and copies the page into its content directory:

```kdl
from "docker.io/redhat/ubi9:latest"
run "dnf install -y nginx && dnf clean all"
copy "index.html" "/usr/share/nginx/html/index.html"
expose "80/tcp"
cmd {
    exec "nginx" "-g" "daemon off;"
}
```

`run` executes the package installation in the image. `copy` adds your page, `expose` records the HTTP port, and `cmd` starts NGINX in the foreground. `index.html` is an ordinary HTML page beside the definition; the example displays “Hello from Coopr.”

The base is [Red Hat's UBI9 image on Docker Hub](https://hub.docker.com/r/redhat/ubi9/).

## Build and run

```sh
coopr build examples/first-image/image.coopr --tag coopr-demo:base
coopr copy coopr-demo:base podman:localhost/coopr-demo:base
podman run --rm -p 127.0.0.1:8080:80 localhost/coopr-demo:base
```

Open [http://localhost:8080](http://localhost:8080) to see the page. Press Ctrl+C to stop the server; `--rm` removes the stopped container.

The build names the image in Coopr's selected local store. `coopr copy` transfers it to Podman, which runs the container. If your [default store is Podman](../guides/storage.md), the built image is already available there and you can run it directly.

## Change the page

Edit `examples/first-image/index.html`, repeat the build and copy commands, then start the container again. The changed file produces a new COPY result; unchanged inputs can reuse the instruction cache.

Keep the local `coopr-demo:base` image for the [component tutorial](reusable-components.md).
