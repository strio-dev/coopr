# Build with Nix inputs

Use Nix packages in a Coopr image or reusable component. Nix prepares the packages and their runtime dependencies; Coopr builds the result outside the Nix sandbox using your normal [Coopr runtime](../getting-started/index.md).

## Before you start

Use Linux with Nix flakes enabled and a working [rootless Coopr host](../getting-started/index.md#set-up-your-host). Podman runs the finished image. Nix provides the build inputs and the launcher's Coopr executable; it does not configure the host's container runtime.

The standalone `coopr` commands later in this guide also require [Coopr on your PATH](../getting-started/index.md#install-with-nix). Running a generated launcher does not install that command in your shell.

## Declare an image

In a new directory, save this as `flake.nix`. The example selects an x86-64 Linux package set; use `aarch64-linux` on ARM64:

```nix
{
  inputs.coopr.url = "github:strio-dev/coopr";
  inputs.nixpkgs.follows = "coopr/nixpkgs";

  outputs = { coopr, nixpkgs, ... }: let
    system = "x86_64-linux";
    pkgs = import nixpkgs {
      inherit system;
      overlays = [ coopr.overlays.default ];
    };
  in {
    packages.${system} = {
      environment = pkgs.coopr.buildImage {
        name = "my-environment";
        tag = "dev";
        packages = [ pkgs.bash pkgs.coreutils ];
        config.Cmd = [ "/bin/bash" ];
      };
    };
  };
}
```

Prepare the inputs, then build and run the image:

```sh
nix build .#environment
nix run .#environment -- --iidfile image.id --metadata-file image.json
podman run --rm localhost/my-environment:dev /bin/bash -c 'printf "Hello from Nix!\n"'
```

The container prints `Hello from Nix!`.

`nix build` prepares the recipe and its inputs. `nix run` invokes `coopr build` and stores the final image locally; it does not push to a registry. Run Podman with the same user and storage settings as Coopr.

The package set selects the platform: `x86_64-linux` produces `linux/amd64` inputs and `aarch64-linux` produces `linux/arm64`. Running foreign binaries requires an already-configured emulator or a native runner.

## Apply components

Add `components` inside the `pkgs.coopr.buildImage` attributes above, listing digest-pinned references in the order they should run:

```nix
components = [
  {
    ref = "registry.example/team/tooling@sha256:REPLACE_WITH_REAL_DIGEST";
    args.tool = "${pkgs.hello}/bin/hello";
    # archive = ./tooling.oci.tar;
  }
];
```

Replace the placeholder with the component's digest. An optional `archive` loads the component locally and verifies that it matches `ref`. Without it, Coopr resolves the reference normally. Component instructions retain their normal behavior, including argument scope and changes to the final image configuration.

Nix includes the closure of `hello` even when it appears only in an argument. Keep Nix string contexts intact: a plain string that resembles a store path does not declare a dependency. Pin nested component and image references too when reproducibility matters; one component archive does not necessarily contain its dependencies.

## Use a native definition

Provide `definition` and `context` to build from your own Coopr file:

```nix
pkgs.coopr.buildImage {
  name = "development";
  packages = [ pkgs.bash pkgs.coreutils ];
  definition = ./image.coopr;
  context = ./.;
  args.message = "hello";
}
```

In `image.coopr`, select the reserved Nix image context:

```kdl
arg "message" "hello"
from "coopr-nix" {
    arg "message"
    run "echo '${message}' > /message.txt"
}
```

`definition` and generated `components` are mutually exclusive. `args` and `target` apply to an explicit definition. Without `context`, the primary context is empty. Static definition, context and archive inputs must be Nix paths, derivations or store-path strings retaining their Nix context.

## Closures and configuration

`packages` links executables under `/bin` and includes their original store paths and transitive runtime dependencies. Paths referenced through `copyToRoot`, configuration or arguments also retain their closures. `copyToRoot`, `perms` and `maxLayers` configure the initial input image through nix2container.

`config` sets initial OCI image configuration. Its default environment is `[ "PATH=/bin" ]`; explicit `config.Env` replaces it. Supported fields are `User`, `ExposedPorts`, `Env`, `Entrypoint`, `Cmd`, `Volumes`, `WorkingDir`, `Labels`, `StopSignal` and `ArgsEscaped`. Components can change these values later. No shell, user database, certificates or Nix installation is added implicitly.

Values written in the Nix definition enter the Nix store, where other users on the machine can read them. Supply secrets through runtime Coopr options or authentication files instead.

## Package Nix tools as a reusable component

Add a `tools` package beside `environment`, inside the `packages.${system}` set in the flake above. `buildComponent` prepares a recipe that runs `coopr component build`:

```nix
tools = pkgs.coopr.buildComponent {
  name = "nix-tools";
  tag = "dev";
  packages = [ pkgs.bash pkgs.coreutils pkgs.hello ];
};
```

```sh
nix build .#tools
nix run .#tools -- --metadata-file tools.json
coopr component save nix-tools:dev --output nix-tools.oci.tar
```

The component carries the complete input filesystem, including `/nix/store` and executable symlinks. Invoking it copies those files into the caller without changing the caller's environment, workdir, command or entrypoint. Use the same native store settings for the build and subsequent save.

Load the archive with an explicit local name:

```sh
coopr component load --input nix-tools.oci.tar --tag nix-tools:dev
```

Use that name in a native definition:

```kdl
from "scratch" {
    component "nix-tools:dev"
    run { exec "/bin/hello"; }
}
```

A generated component can also declare `components`. Their instructions execute when the parent is invoked. For custom PACKAGE or EXTEND behavior, supply a native `definition` instead. Nonempty OCI `config` is rejected for components; write configuration changes in EXTEND.

### Deliver local dependencies

When a component uses children loaded from archives, pass `--bundle-output` to package their dependencies in a complete Bale bundle. If a child was delivered through Bale, supply its original complete bundle with `bundle`:

```nix
components = [
  {
    ref = "registry.example/team/configure@sha256:REPLACE_WITH_REAL_DIGEST";
    archive = ./configure.oci.tar;
    bundle = ./configure.bale;
  }
];
```

```sh
nix run .#tools -- --bundle-output ./tools.bale
bale inspect --layout ./tools.bale
```

A locally built child that has not been delivered through Bale needs no prior bundle if its dependencies are otherwise available. Missing or conflicting dependencies fail packaging. `bundle` is only supported by the component builder. Deliver the complete bundle rather than pushing the parent alone. See [component bundles](../reference/cli.md#complete-component-bundles).

## Outputs and runtime options

Both builders expose `inputImage`, `inputImage.oci`, `definition`, `platform`, `recipe` and `artifact`. `inputImage.oci` is the initial Nix image, not the composed image or component archive. Export the final image with ordinary Coopr:

```sh
coopr image push "$(cat image.id)" oci:./image-output:image
```

Forward native options after `--`, including cache, storage, runtime and metadata settings. The launcher protects its definition, platform, reserved context and declared arguments against conflicting overrides. Use separate short options and values rather than combined tokens.

The overlay's Coopr executable is selected by default. `cooprBinary = "${anotherCoopr}/bin/coopr"` selects a different Nix package; `COOPR_BINARY` overrides execution at runtime. A runtime override does not remove the declared Nix dependency.

Nix caches packages and the initial input image. Coopr caches build instructions and stores the resulting images and components. See [caching](caching.md) for refresh controls and [the examples](https://github.com/strio-dev/coopr/tree/main/examples/nix) for complete recipes.
