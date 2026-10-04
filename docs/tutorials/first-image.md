# Build your first image

Build a Red Hat UBI9 image that serves your own web page with NGINX.

After [installing Coopr](../getting-started/index.md), clone the repository to use its examples:

```sh
git clone https://github.com/strio-dev/coopr.git
cd coopr
```

Run the commands from the checkout. You need Coopr, Podman, and access to Docker Hub and UBI's public package repositories. No Red Hat subscription is needed.

## Read the definition

`examples/first-image/image.coopr` installs NGINX and copies the adjacent `index.html`:

```kdl
from "docker.io/redhat/ubi9:latest"
run "dnf install -y nginx && dnf clean all"
copy "index.html" "/usr/share/nginx/html/index.html"
expose "80/tcp"
cmd {
    exec "nginx" "-g" "daemon off;"
}
```

The base is [Red Hat's UBI9 image on Docker Hub](https://hub.docker.com/r/redhat/ubi9/). NGINX serves the copied page when the container starts.

## Build and run

```sh
coopr build examples/first-image/image.coopr --tag coopr-demo:base
coopr copy coopr-demo:base podman:localhost/coopr-demo:base
podman run --rm -p 127.0.0.1:8080:80 localhost/coopr-demo:base
```

Open [http://localhost:8080](http://localhost:8080) to see the page. Press Ctrl+C to stop the server; `--rm` removes the stopped container.

Coopr keeps the built image in its selected store. The copy command makes it available to Podman. If [Podman is your default store](../guides/storage.md), skip that copy.

## Change the page

Edit `examples/first-image/index.html`, repeat the build and copy commands, then start the container again. The changed file produces a new COPY result; unchanged inputs can reuse the instruction cache.

Keep the local `coopr-demo:base` image for the [component tutorial](reusable-components.md).
