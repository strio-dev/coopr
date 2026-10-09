# Scripts

Run these tasks from the repository root using the [Nix development environment](../docs/contributing.md).

| Directory | Tools | Recommended task |
| --- | --- | --- |
| `acceptance/` | Packaged image and Docker transfer checks. | `just packaged-acceptance`, `just docker-acceptance` |
| `benchmarks/` | Coopr/Podman build measurements. | `just benchmark` |

`just release-acceptance` runs the same complete NixOS VM checks as `just integration`, including dynamic and static packaged acceptance.

The raw host checks need Linux and rootless Podman. Packaged acceptance also needs a foreign-architecture QEMU binfmt handler with the `F` flag. The NixOS checks provision these requirements. Docker acceptance starts a disposable privileged Docker daemon inside Podman; `COOPR_DOCKER_MODERN_STORE=false` selects its legacy image store.

Benchmarks write CSV to stdout and have no fixed timing gates. Set `COOPR_PARITY_BENCH_MULTIPLATFORM=1` to include foreign-platform runs; those need the corresponding QEMU binfmt handler.
