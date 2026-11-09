# Go execution library

Run builds directly from Go with `github.com/strio-dev/coopr`. This separate module uses the checkout's source through a local `replace` directive.

## Run the example

Start from the repository root on a host configured for [rootless builds](../../docs/guides/security.md#native-rootless-requirements). The pinned shell supplies the native libraries, Go build tags, static BusyBox and `crun`:

```sh
nix develop --no-update-lock-file path:.#runtime
cd examples/library
go run . -busybox "$(command -v busybox)" -network none -work "$PWD/proof"
```

`proof` must not already exist. The example leaves its stores, cache and exported files there for inspection. `-network none` disables networking for RUN instructions; registry access is configured separately.

The example builds a typed program, without parsing a definition or starting the Coopr CLI. It checks that:

- Independent build branches can run together, then copy their results into one image.
- A new image store can reuse the portable cache from a previous build.
- Writable context mounts leave the original input files unchanged.
- Environment values reach RUN unchanged.
- Cancellation stops workers and removes their temporary containers and staging files.
- An export inside the build context does not copy its own staging files into the image or include them in cache keys.

A successful run prints a JSON summary with cache statistics and the results of the concurrency, isolation and cleanup checks. Failures exit with an error. The example removes its first in-context export before repeating that build, so the second run starts with the same inputs.

See the [library guide](../../docs/guides/library.md) for initialization, execution and cleanup in your own application.
