# Independent Go consumer

This module imports only `github.com/strio-dev/coopr`. The local replace
points at the checkout root. Run from a shell with the repository's native
libraries, Go build tags, a static BusyBox, `crun`, and working rootless namespaces:

```sh
nix develop .#runtime
cd examples/library
go run . -busybox "$(command -v busybox)" -work "$PWD/proof"
```

`proof` must not already exist. The consumer leaves its stores, cache and exported
files there for inspection. It verifies independent branch overlap through a
rendezvous, dependent COPY convergence, portable cache hits in a fresh native
store, writable context isolation, literal environment values, and cancellation after readiness with process/container/staging cleanup.
It also checks `COPY "."` while filesystem export is inside the input context:
private staging must stay out of the image and portable cache identity. The
proof removes its cold successful outputs before the fresh-store repeat, so
caller-owned output files are not accidentally treated as unchanged inputs.
It does not invoke the Coopr CLI or parse a Coopr definition.

See [the library guide](../../docs/guides/library.md) for bootstrap and ownership.
