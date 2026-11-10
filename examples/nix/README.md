# Build with Nix packages

`image.nix` combines Bash and coreutils from Nix with the local component in `component.coopr`. Nix builds the input image, then Coopr applies the component outside the Nix sandbox.

## Build an image

Run from the repository root on a host configured for [rootless builds](../../docs/guides/security.md#native-rootless-requirements):

```sh
nix build path:.#coopr --out-link result-coopr
export COOPR_BINARY="$PWD/result-coopr/bin/coopr"
nix build path:.#nix-environment
nix run path:.#nix-environment -- --iidfile image.id --metadata-file image.json
```

The final image is named `coopr-nix-environment:dev`. The component receives `message=nix-and-coopr` and writes `/coopr-component-result`.

Export the finished image to an OCI layout:

```sh
"$COOPR_BINARY" image push "$(cat image.id)" oci:./environment:image
```

Use the same image store for build and export. To inspect the input before Coopr applies the component, run `nix build path:.#nix-environment.inputImage.oci`.

## Produce a reusable component

`component.nix` packages Bash, coreutils, hello and their runtime dependencies. The component copies these files into the calling image and keeps that image's configuration.

```sh
nix build path:.#nix-tools-component
nix run path:.#nix-tools-component -- --network none --metadata-file tools.json
"$COOPR_BINARY" component save coopr-nix-tools:dev --output nix-tools.oci.tar
```

Load the archive into another component store with `coopr component load --input nix-tools.oci.tar`. This component needs no other components. To include components it calls from local archives, follow [Deliver local dependencies](../../docs/guides/nix.md#deliver-local-dependencies).

See [the Nix guide](../../docs/guides/nix.md) for overlay setup, native definitions, configuration and cache options.
