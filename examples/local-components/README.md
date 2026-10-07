# Local components

The API and worker definitions share `build/components/shared.coopr`. Each invokes it with a different `channel` argument; no component build or upload is needed.

```sh
coopr build -f examples/local-components/build/api.coopr examples/local-components/build --tag coopr-local:api
coopr build -f examples/local-components/build/worker.coopr examples/local-components/build --tag coopr-local:worker
coopr copy coopr-local:api podman:localhost/coopr-local:api
podman run --rm localhost/coopr-local:api
```

Paths resolve within the `build/` context. The component packages `components/settings.conf` from that context, then installs it in each image. See [local component paths](../../docs/guides/components.md#share-a-component-within-a-repository).
