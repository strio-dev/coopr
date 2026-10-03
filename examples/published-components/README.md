# Published components

Package company defaults and a Go formatting tool, then consume both artifacts from Coopr's local component store:

```sh
coopr component build examples/published-components/components/company-config/component.coopr --target debug --tag coopr-company-config
coopr component build examples/published-components/components/gofmt/component.coopr --tag coopr-gofmt
coopr build examples/published-components/image.coopr --tag coopr-company:formatted
coopr copy coopr-company:formatted podman:localhost/coopr-company:formatted
podman run --rm --network=none localhost/coopr-company:formatted
```

The unformatted `main.go.in` input is copied to `main.go` and formatted during the image build. The container prints that formatted source and the packaged defaults. The company component keeps its `debug` and `runtime` outputs; `--target debug` fixes the published artifact's output, while `channel="preview"` is supplied at invocation. The tool component compiles `gofmt` during packaging and copies that packaged executable into the caller's image.

These commands build reusable artifacts locally; they do not publish to a registry. See [components](../../docs/guides/components.md) and [stages and phases](../../docs/concepts/stages.md) for artifact references and package timing.
