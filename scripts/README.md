# Scripts

Run these tasks from the repository root using the [Nix development environment](../docs/contributing.md).

| Directory | Tools | Recommended task |
| --- | --- | --- |
| `acceptance/` | Packaged image and Docker transfer checks. | `just packaged-acceptance`, `just docker-acceptance` |
| `benchmarks/` | Coopr/Podman build measurements. | `just benchmark` |

`just release-acceptance` runs the same complete NixOS VM checks as `just integration`, including dynamic and static packaged acceptance.

The packaged-image and Docker host checks need Linux and rootless Podman. Packaged acceptance also needs a foreign-architecture QEMU binfmt handler with the `F` flag. The NixOS checks provision these requirements. Docker acceptance starts a disposable privileged Docker daemon inside Podman; `just docker-acceptance classic` selects its legacy image store.

Run host checks with an isolated Podman store and no competing image writers;
loading test archives can add image tags. Use Just to select the required build
tags and runtime tools.

`just examples-acceptance` packages and consumes example components through a temporary local registry, then runs the image. It needs network access for upstream inputs but no registry account. `just cosign-acceptance` runs external signature verification with an installed Cosign CLI. These suites use Go build tags selected by their Nix shells; the ordinary rootless suites run without Go's `-short` flag.

`just nix-image-acceptance /absolute/new-results` and `just nix-component-acceptance /absolute/new-results` build this checkout and check native Nix composition on a configured rootless host. They use the pinned development tools and `/var/tmp` for scratch; pass a second path to choose another disk-backed scratch directory. Results are retained for inspection. Successful runs remove their private scratch using Podman; failed runs retain it, with its path recorded in the result directory.

Benchmarks write CSV to stdout and have no fixed timing gates. Run `just benchmark 5 1` for five iterations including foreign-platform builds; those need the corresponding QEMU binfmt handler. The defaults are five iterations and native builds only. Use Just to run the script in the pinned Nix environment.
