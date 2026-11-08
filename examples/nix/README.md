# Nix inputs and native Coopr composition

`image.nix` combines Nix-built Bash/coreutils with the local component in `component.coopr`. Nix prepares the complete input image; the runnable package invokes Coopr outside the build sandbox.

## Build an image

From the Coopr checkout, with the normal rootless runtime configured:

```sh
nix build path:.#coopr --out-link result-coopr
export COOPR_BINARY="$PWD/result-coopr/bin/coopr"
nix build path:.#nix-environment
nix run path:.#nix-environment -- --iidfile image.id --metadata-file image.json
```

The final image is named `coopr-nix-environment:dev`. The component receives `message=nix-and-coopr` and writes `/coopr-component-result`.

Export the composed result to a complete OCI layout:

```sh
"$COOPR_BINARY" image push "$(cat image.id)" oci:./environment:image
```

Use the same native store settings for build and export. The initial input alone is available with `nix build path:.#nix-environment.inputImage.oci`; it does not contain component changes.

## Produce a reusable component

`component.nix` packages Bash, coreutils and hello with their complete runtime closures. The resulting component copies files into its caller while preserving the caller's image configuration.

```sh
nix build path:.#nix-tools-component
nix run path:.#nix-tools-component -- --network none --metadata-file tools.json
"$COOPR_BINARY" component save coopr-nix-tools:dev --output nix-tools.oci.tar
```

Load the archive into another component store with `coopr component load --input nix-tools.oci.tar`. This tools package has no nested dependencies. For components with archive-selected children, use complete bundle output as described in [the Nix guide](../../docs/guides/nix.md#deliver-local-dependencies).

See [the Nix guide](../../docs/guides/nix.md) for overlay setup, native definitions, configuration and cache options.
