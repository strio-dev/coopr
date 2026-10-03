# Reusable components

Build the [first image](../first-image/README.md) before applying these settings and policy components:

```sh
coopr component build examples/reusable-components/components/settings/component.coopr --tag coopr-demo-settings
coopr component build examples/reusable-components/components/policy/component.coopr --tag coopr-demo-policy
coopr build examples/reusable-components/image.coopr --tag coopr-demo:configured
coopr copy coopr-demo:configured podman:localhost/coopr-demo:configured
podman run --rm --network=none localhost/coopr-demo:configured
```

The container prints the packaged settings and policy. The [two-component tutorial](../../docs/tutorials/reusable-components.md) explains the package stages and invocation order.
