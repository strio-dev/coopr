# Contributing

## Set up

From a checkout, enter the pinned Nix development shell:

```sh
nix develop path:.
```

Direnv can load the same shell with `direnv allow`. The shell provides Go, Just, linters, and a packaged Coopr CLI. Use Go to run source changes immediately:

```sh
go run ./cmd/coopr --help
go run ./cmd/coopr build examples/first-image/image.coopr --tag coopr-demo:base
```

Read the [architecture](concepts/architecture.md) and [execution reference](reference/execution.md) before changing the planner or executor. Add regression coverage for behavior changes.

## Check changes

| Command | Checks |
| --- | --- |
| `just test` | Go tests; live tests skip unless configured. |
| `just vet` / `just lint` | Go static analysis. |
| `just fmt` | Go, Nix, and Justfile formatting. |
| `just check` | Local flake checks, including untracked files. |
| `just release-check` | Flake checks using the tracked Git source. |

GitHub Actions runs quality and documentation checks once on amd64, and native builds/tests on amd64 and arm64. `nix/ci.nix` assigns checks to these categories and generates their matrices through `nix-github-actions`. Register new checks in the relevant category.

## Edit documentation

```sh
just docs-serve
just docs-check
```

The preview stays on localhost. `docs-check` builds the site, parses public KDL examples, and tests the installer with local fixtures. The Nix docs check also verifies that the installer reaches the site unchanged.

Add pages to `zensical.toml`. Keep tutorials runnable from a fresh checkout and use the existing syntax and assets.

## Run integration checks

`just integration` and `just release-acceptance` require an AMD64 host with KVM. One NixOS VM runs live integration tests and both dynamic and static packaged CLI acceptance as a normal user, including foreign-architecture execution.

The raw `packaged-acceptance`, `docker-acceptance`, and `benchmark` commands need a configured Linux host with working rootless containers:

| Command | Scope |
| --- | --- |
| `just integration` | AMD64 NixOS VM: live Buildah, registry, native-storage tests, and dynamic/static packaged acceptance. |
| `just packaged-acceptance` | Packaged image, store reuse, multi-platform components, and Podman comparison. |
| `just release-acceptance` | The same complete AMD64 NixOS VM check as `just integration`. |
| `just docker-acceptance` | Docker transfers using a disposable daemon. |
| `just benchmark` | Coopr/Podman timing measurements. |

See [security](guides/security.md) for namespace/runtime requirements. Skipped integration tests do not establish runtime support.

## Releases

After checks pass, push an unprefixed version tag such as `1.2.3` or `1.2.3-rc.1`. CI takes the build version from that tag and publishes Linux binary archives, corresponding sources, checksums, and a multi-platform GHCR image. No manual version bump is required.

Prereleases keep their versioned tags; only the latest stable release updates the container's `latest` alias. Container tags replace `+` with `_` for build metadata, while the binary retains the exact release version. Documentation deploys from `main` after its checks pass.

### Corresponding source

Each release provides `coopr-sources.tar.gz` and a matching `ghcr.io/strio-dev/coopr:source-<version>` image containing the same source bundle. The runtime image's `org.opencontainers.image.source` label links to that release's `coopr-sources.tar.gz` asset. Source images are extracted, not run.

With [regctl](https://regclient.org/install/), extract a release's source bundle directly from GHCR:

```sh
mkdir coopr-sources
regctl image get-file ghcr.io/strio-dev/coopr:source-1.2.3 /coopr-sources.tar.gz | tar -xz -C coopr-sources
```

Or download `coopr-sources.tar.gz` from the matching GitHub release and extract it locally:

```sh
mkdir coopr-sources
tar -xzf coopr-sources.tar.gz -C coopr-sources
```

Read `coopr-sources/REBUILD.txt` for the bundled application, dependency sources, patches, pinned build recipes, and instructions for rebuilding with modified libraries. Source archives are checksummed with the release binaries.
