# Reusable components

Package the settings and policy components, then apply them to Red Hat UBI9 from Docker Hub. Run these commands from the repository root:

```sh
coopr component build examples/reusable-components/components/settings/component.coopr --tag coopr-demo-settings
coopr component build examples/reusable-components/components/policy/component.coopr --tag coopr-demo-policy
coopr build examples/reusable-components/image.coopr --tag coopr-demo:configured
coopr copy coopr-demo:configured podman:localhost/coopr-demo:configured
podman run --rm localhost/coopr-demo:configured
```

The container prints the packaged settings and policy. The [two-component tutorial](../../docs/tutorials/reusable-components.md) explains the package stages and invocation order.
