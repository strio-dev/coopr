# Local components

Two image definitions share `build/components/shared.coopr`. Their `./components/shared.coopr` references resolve within the `build/` context; the component packages `components/settings.conf` from that same context.

```sh
coopr build -f examples/local-components/build/api.coopr examples/local-components/build --tag coopr-local:api
coopr build -f examples/local-components/build/worker.coopr examples/local-components/build --tag coopr-local:worker
coopr copy coopr-local:api podman:localhost/coopr-local:api
podman run --rm --network=none localhost/coopr-local:api
```

Both builds apply the shared settings automatically, with different `channel` arguments. No separate component build or upload is required. See the [local-component guide](../../docs/guides/components.md#share-a-component-within-a-repository) for path rules.
