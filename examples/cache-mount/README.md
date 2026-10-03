# Cache mount

Compile `gofmt` using Go's build cache:

```sh
coopr build examples/cache-mount/image.coopr --tag coopr-cache:gofmt
coopr build examples/cache-mount/image.coopr --tag coopr-cache:gofmt --no-cache
```

`--no-cache` reruns the instruction while the cache mount retains compiler outputs. The cache is build storage; it does not become part of the resulting image. See [caching](../../docs/guides/caching.md) for cache configuration.
