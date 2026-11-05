# Nix inputs and native Coopr composition

`image.nix` combines Nix-built Bash/coreutils with the existing local component
in `component.coopr`. Nix prepares the complete input image; the runnable
package invokes ordinary Coopr outside the build sandbox.

From the Coopr checkout, with the normal rootless Coopr runtime configured:

```sh
nix build path:.#coopr --out-link result-coopr
export COOPR_BINARY="$PWD/result-coopr/bin/coopr"
nix build path:.#nix-environment
nix run path:.#nix-environment -- --iidfile image.id --metadata-file image.json
```

`nix build` prepares inputs and the launcher. `nix run` performs native
composition and names the final image `coopr-nix-environment:dev`. The native
component receives `message=nix-and-coopr`, preserves the input environment,
and writes `/coopr-component-result`. It is not executed by Nix.

Use an isolated Coopr store when qualifying changes; pass the same `--root`,
`--runroot` and `--storage-driver` options to both build and export. The commands
above deliberately use the ordinary persistent store and do not prune it.

## Consume the final image with Stoke

Export the composed native result into a dedicated context:

```sh
context=$(mktemp -d)
"$COOPR_BINARY" image push "$(cat image.id)" "oci:$context/environment:image"
stoke run -f examples/nix/stoke.yaml --context "$context" \
  --coopr-binary "$COOPR_BINARY"
```

This local OCI export is not registry publication. `stoke.yaml` runs fresh
checks of the final component output and Nix-provided tools. It does not run
Nix on the host or mount its daemon/store into the container. The exported
layout is complete and can be consumed by an independent store/machine.
Remove your copied context when finished; persistent Coopr storage has its
ordinary lifecycle.

The initial input alone is available as
`nix build path:.#nix-environment.inputImage.oci`. It intentionally contains
neither component mutations nor `/coopr-component-result`.

## Build and test Stoke itself

`stoke-image.nix` accepts `{ pkgs, stokeSource }`, where `pkgs` has the Coopr
overlay and `stokeSource` is an explicit Stoke checkout path. It reuses that
checkout's pinned Go package, exact source and Nix-generated vendor output.
`go-image.coopr` applies `go-component.coopr` through Coopr to compile Stoke.
The Go executable is passed as a native argument; its runtime closure is
included automatically without a second package list.

After composing and exporting that final image as `environment:image`,
`stoke-go.yaml` runs the actual source's short Go suite with `-count=1`, plus
the newly built Stoke executable. Tests are fresh; Go caches are disposable
acceleration. This is distinct from running Nix flake checks.

## Run the acceptance tests

With Nix and the ordinary rootless runtime already provisioned, run the small
native suite and the real Stoke workload from the Coopr checkout:

```sh
python3 scripts/acceptance/nix-frontend.py \
  --coopr-binary "$COOPR_BINARY" \
  --evidence /path/to/new/frontend-evidence --scratch-parent /path/to/scratch

python3 scripts/acceptance/nix-image.py \
  --coopr-source "$PWD" --stoke-source /path/to/stoke \
  --coopr-binary "$COOPR_BINARY" --stoke-binary /path/to/development/stoke \
  --evidence /path/to/new/go-evidence --scratch-parent /path/to/scratch \
  --scratch-limit-gib 12
```

Choose an executable scratch filesystem with sufficient space and memory for
the native stores. On the tested VFS runner, the small suite used less than
2 GiB and the Go workload needed roughly 10 GiB. The Go harness enforces a configurable bound,
retains logs and private scratch for diagnosis, and does not delete user data
or prune existing stores. If the runtime uses nonstandard helper paths, see
each command's `--help` for the supported runtime options.

These tests cover native composition, closure-only arguments, invalidation,
warm and independent-store OCI cache reuse, cancellation, and fresh source
tests. They do not qualify native ARM, another UID, GHCR transport or actual
Nix flake checks inside Stoke. The frontend suite can additionally consume a
pair of compatible Bale-generated delivery fixtures through its optional
`--delivery-archive-a` and `--delivery-archive-b` arguments.

The implementation is unreleased and the engine remains unpinned. See
[the Nix guide](../../docs/guides/nix.md) for the consumer overlay, arguments,
platform, cache ownership and flake-check execution boundary.
