# Build Nix inputs, compose with Coopr

`pkgs.coopr.buildImage` prepares Nix inputs and a runnable Coopr recipe. Nix
builds packages and their complete runtime closures. Running the recipe invokes
ordinary Coopr **outside the Nix build sandbox**, where Coopr applies native
components and produces the final image. No VM or nested Buildah is required.

This interface is unreleased. Use the local development flake and executable;
a compatible published engine has not been pinned. The launcher needs the same
Linux rootless runtime setup as the ordinary Coopr CLI. It does not install
helpers, allocate subordinate IDs, or change host configuration.

## Declare an image

Apply the development flake's overlay to your package set:

```nix
{
  inputs.coopr.url = "path:/path/to/coopr-stoke-api";
  inputs.nixpkgs.follows = "coopr/nixpkgs";

  outputs = { coopr, nixpkgs, ... }: let
    system = "x86_64-linux";
    pkgs = import nixpkgs {
      inherit system;
      overlays = [ coopr.overlays.default ];
    };
  in {
    packages.${system}.environment = pkgs.coopr.buildImage {
      name = "my-environment";
      tag = "dev";
      packages = [ pkgs.bash pkgs.coreutils ];
      config = {
        Env = [ "PATH=/bin" ];
        WorkingDir = "/";
      };
    };
  };
}
```

The local flake path is a development input, not a published engine version.
The package set determines the target: `x86_64-linux` means `linux/amd64`, and
`aarch64-linux` means `linux/arm64`. The launcher does not provision foreign
builders or emulation. Runtime flags cannot replace that platform or the
reserved `coopr-nix` image context.

Native definitions and components can still make explicit stage selections;
their resulting platform is reported by Coopr. `platform` on the Nix result
describes the requested Nix input platform, not an override of native semantics.

```sh
nix build .#environment
nix run .#environment -- --iidfile image.id --metadata-file image.json
```

The first command prepares the launcher and its Nix inputs; it does **not**
execute components or produce the final image. The second invokes native
`coopr build`. A declared `tag` names the final native image as `name:tag`;
no registry push is implicit. Native metadata distinguishes the image ID from
manifest and configuration digests.

## Apply existing components

Declare components in invocation order, using immutable references:

```nix
components = [
  {
    ref = "registry.example/team/tooling@sha256:REPLACE_WITH_REAL_DIGEST";
    args = {
      channel = "development";
      tool = "${pkgs.hello}/bin/hello";
    };
    # archive = ./tooling.oci.tar; # optional local component delivery
  }
];
```

Replace the example reference with a real SHA-256 digest. With `archive`, the
launcher uses Coopr's existing component loader and inspects the loaded root
identity before building. Each immutable archive has a stable, explicit local
alias, retaining its own delivery context even when several deliveries share
the same component digest. The archive selects that delivery; `ref` supplies
the expected immutable root. Without an archive, the qualified reference is
passed to Coopr unchanged. Loading an archive does not execute its component.
An identity mismatch stops composition; imported immutable content may remain
cached in Coopr's component store.

The generated native definition starts from `coopr-nix` and applies the
components without translating their instructions. Native argument scope,
PACKAGE/EXTEND behavior, mounts, dependency planning and execution remain
Coopr's responsibility. A component may modify the caller or select an
independent image. The final filesystem and configuration follow those native
semantics; the initial Nix configuration is not reapplied afterward.

A component root digest alone does not pin all its external dependencies.
Coopr resolves nested components, images and artifacts normally, including
argument-selected inputs. Reproducibility requires those inputs to be pinned
and the operations themselves to be reproducible. A single component archive
is not necessarily a complete offline dependency bundle. `--pull=never` is
not a blanket prohibition on component registry access.

## Use a native definition

For local components or more involved native composition, provide a definition
and an explicit context instead of the generated `components` list:

```nix
pkgs.coopr.buildImage {
  name = "development";
  packages = [ pkgs.bash pkgs.coreutils ];
  definition = ./image.coopr;
  context = ./.;
  args.message = "development";
  # target = "selected-stage";
}
```

```kdl
arg "message" "development"
from "coopr-nix" {
    arg "message"
    component "./components/configure.coopr" {
        arg "message" "${message}"
    }
}
```

The native definition explicitly selects the reserved Nix image context.
`definition` and generated `components` are mutually exclusive. When no
primary `context` is declared, it is empty: the launch directory is not silently
captured as an input. Explicit Nix paths are snapshotted by Nix; runtime native
contexts remain subject to Coopr's normal access and cache rules. Static
`definition`, `context` and component `archive` inputs accept Nix paths,
derivations, or canonical store-path strings retaining their Nix context;
context-bearing strings naming arbitrary host paths or using `..` are rejected.

## Closures and configuration

`packages` links executables under `/bin`; their original store paths and
transitive runtime dependencies are included in the input image. Paths
referenced through `copyToRoot`, image configuration or declared arguments also
retain their Nix closures. A package used only through an argument does not
need a second package list. Keep Nix string contexts intact: an ordinary
string that merely resembles a store path does not declare a dependency.

`config` is initial OCI image configuration. Its default environment is
`[ "PATH=/bin" ]`; explicit `config.Env` replaces that list. No shell, user
database, certificates or Nix installation is added implicitly. `copyToRoot`,
`perms` and `maxLayers` use the existing nix2container input-image machinery.
Static image ownership is represented in layer metadata; Coopr handles actual
execution normally after Nix finishes.

The result exposes:

| Attribute | Meaning |
| --- | --- |
| The runnable package | Launcher and declared Nix inputs; `nix run` composes the image. |
| `inputImage` | Upstream lazy description of the initial Nix image. |
| `inputImage.oci` | Complete OCI layout of that initial input, with local reference `image`. |
| `definition` | Native recipe passed to Coopr. |
| `platform` | Package-set image platform. |

There is no top-level `.oci` claiming to contain the final composed image.
A lazy description's size is not the size of its referenced closure or its
standalone export. The final native image can be exported through ordinary
`coopr image push IMAGE_ID oci:OUTPUT:image`; this local transport does not
publish to a registry.

All static attributes, including arguments and configuration, enter the
readable Nix store. Keep credentials and secrets in runtime native options,
authentication files or environment variables rather than these attributes.

## Cache and lifecycle

Nix caches package outputs and input-image derivations. Coopr owns the native
composition cache and its existing local/OCI cache backends. Forward native
settings such as `--cache-from`, `--cache-to`, `--root`, `--runroot`,
`--storage-driver`, `--runtime`, `--iidfile`, and `--metadata-file` after `--`.
Authentication and security policy remain ordinary Coopr invocation settings.
`COOPR_BINARY` explicitly selects a development executable when needed.

The launcher protects its declared definition, platform, reserved context and
argument bindings from conflicting runtime overrides. Use separate short
options and their values; combined short-option tokens are rejected. It does
not set a special runtime, network policy, fixed timestamp, writable host bind,
cache policy or push operation. Final build execution replaces the launcher
process so signals reach Coopr. Native stores and caches are persistent
user-owned state; the launcher does not prune them.

Nix output reuse does not prove that a test executed again. Likewise, arbitrary
component RUN operations may be nondeterministic. Use native fresh-only policy
for tests and required effects, and inspect results rather than treating a
substitution or cache hit as fresh execution.

## Stoke and flake checks

The checked-in `examples/nix/README.md` prepares inputs with Nix, composes
them through ordinary Coopr, and gives Stoke the resulting complete OCI image.
Stoke continues using its existing Coopr-backed task executor for containers.
Its separately implemented generic host tasks can run provisioned tools with
explicit caller authority; this image integration adds no Nix-specific executor.

A private container-owned Nix store was tested with sandboxing required and no
host daemon/store mount. Nix evaluated the fixture flake, then failed before
the check builder because the required sandbox namespaces were unavailable
under the ordinary container confinement. Sandboxing was not disabled. That
path does not currently qualify actual container-local flake-check execution.

Flake checks remain Nix derivations. For immediate use, run an explicit check
step with the runner's already-provisioned Nix, then compose the image and run
Stoke. For example, Stoke's actual workflow lint check can gate composition:

```sh
nix build --no-link --no-update-lock-file \
  path:/path/to/stoke#checks.x86_64-linux.workflows
nix run .#environment -- --iidfile image.id
# Export the composed image, then invoke the ordinary Stoke workflow.
```

This is a visible runner/CI step. A Nix substitution counts as a successful
check derivation, not evidence that its checker ran freshly. The runner owns
the Nix installation, supported system, credentials and binary-cache setup.
Use native CI dependencies to gate subsequent jobs; no host execution is
silently added to a Stoke task. VM/KVM checks are separate.

Stoke's generic host-task milestone is complete in the development checkout.
Its `scripts/nix-workflow.py` discovers checks and supported systems from Nix,
and `scripts/run-nix-check.py` records exact checker-build, substitution and
already-valid evidence. A handwritten caller must grant `--allow-host` and
attest required runner capabilities. Imported workflows cannot grant authority.
Fresh host execution is the default; reusable captured outputs need an explicit
complete input/toolchain/effect contract. Native CI jobs and matrix variants
remain visible. See the development Stoke host guide for trust, provisioning,
capture limits and portability requirements; published versions are not implied.
The withdrawn Nix-specific draft remains archived and is not resumed.

## Reuse boundary

The Nix input serializer currently reuses pinned nix2container Nix machinery.
Selective use of its Go `nix` package was also tested in a separate module:
configuration and raw layers matched, while gzip representations differed.
A fixed prebuilt helper exported deterministically inside the Nix sandbox;
its hermetic source packaging and production lifecycle handling remain
unimplemented. No nix2container Go dependency was added to Coopr core.

This optimization does not replace native component execution. Coopr retains
the final image's complete configuration and platform data; its output must
not be round-tripped through a narrower input-image model. For Strata, the
possible seam is standard OCI content carrying a complete Nix-produced
closure. Nix still owns evaluation, package builds and its cache.
