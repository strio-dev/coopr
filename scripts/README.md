# Scripts

Run the `just` tasks from the repository root; they use the pinned Nix development environment.

| Directory | Tools | Recommended task |
| --- | --- | --- |
| `acceptance/` | Packaged CLI image, Docker transfers, and paired Containerfile/Coopr fixtures. | `just packaged-acceptance`, `just docker-acceptance` |
| `benchmarks/` | Optional Coopr/Podman cold, warm, changed-step, component, and concurrent build measurements. | `just benchmark` |

`just release-acceptance` runs `just integration` followed by `just packaged-acceptance`.

Live container checks need Linux and working rootless Podman. Release acceptance also needs an enabled foreign-architecture QEMU binfmt handler with the `F` flag. Image pulls require registry access. Docker acceptance starts a disposable privileged Docker daemon inside Podman; `COOPR_DOCKER_MODERN_STORE=false` selects its legacy store instead of the default modern store.

The benchmark runs on amd64 or arm64 and writes CSV measurements to stdout. Results depend on the host; there are no fixed timing gates. Set `COOPR_PARITY_BENCH_MULTIPLATFORM=1` to include foreign-platform runs; those require the corresponding QEMU binfmt handler.
