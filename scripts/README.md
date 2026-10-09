# Scripts

Run these tasks from the repository root using the [Nix development environment](../docs/contributing.md).

| Directory | Tools | Recommended task |
| --- | --- | --- |
| `acceptance/` | Packaged image and Docker transfer checks. | `just packaged-acceptance`, `just docker-acceptance` |
| `benchmarks/` | Coopr/Podman build measurements. | `just benchmark` |

`just release-acceptance` runs the same complete NixOS VM checks as `just integration`, including dynamic and static packaged acceptance.

The raw host checks need Linux and rootless Podman. Packaged acceptance also needs a foreign-architecture QEMU binfmt handler with the `F` flag. The NixOS checks provision these requirements. Docker acceptance starts a disposable privileged Docker daemon inside Podman; `just docker-acceptance classic` selects its legacy image store.

`just examples-acceptance` runs the online published-component workflow. `just cosign-acceptance` runs external signature verification with an installed Cosign CLI. These suites use Go build tags selected by their Nix shells; the ordinary rootless suites run without Go's `-short` flag.

Benchmarks write CSV to stdout and have no fixed timing gates. Run `just benchmark 5 1` for five iterations including foreign-platform builds; those need the corresponding QEMU binfmt handler. The defaults are five iterations and native builds only. Use Just to run the script in the pinned Nix environment.
