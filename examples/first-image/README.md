# First image

Build an NGINX image with a static page:

```sh
coopr build examples/first-image/image.coopr --tag coopr-demo:base
coopr copy coopr-demo:base podman:localhost/coopr-demo:base
podman run --rm -p 127.0.0.1:8080:80 localhost/coopr-demo:base
```

Open <http://localhost:8080>. See the [first-image tutorial](../../docs/tutorials/first-image.md) for the definition and build steps.
