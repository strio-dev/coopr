# Published components

Package company defaults and a Go formatting tool, then consume both artifacts from Coopr's local component store:

```sh
coopr component build examples/published-components/components/company-config/component.coopr --target debug --tag coopr-company-config
coopr component build examples/published-components/components/gofmt/component.coopr --tag coopr-gofmt
coopr build examples/published-components/image.coopr --tag coopr-company:formatted
coopr copy coopr-company:formatted podman:localhost/coopr-company:formatted
podman run --rm --network=none localhost/coopr-company:formatted
```

The container prints the formatted `main.go` and company defaults. The company component offers `debug` and `runtime` outputs; `--target debug` selects the output for this artifact. Invocation supplies `channel="preview"`. The tool component compiles `gofmt` once during packaging and copies it into the consuming image.

These commands build reusable artifacts locally; they do not publish to a registry. See [components](../../docs/guides/components.md) and [stages and phases](../../docs/concepts/stages.md) for artifact references and package timing.
