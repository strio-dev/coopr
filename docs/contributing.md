# Contributing

## Set up

Development requires Linux and Nix. From a checkout, enter the pinned development shell:

```sh
nix develop path:.
```

Direnv can load the same shell with `direnv allow`. The shell provides Go, Just, linters, and a packaged Coopr CLI. Use Go to run source changes immediately:

```sh
go run ./cmd/coopr --help
go run ./cmd/coopr build examples/first-image/image.coopr --tag coopr-demo:base
```

Read the [architecture](concepts/architecture.md) and [execution reference](reference/execution.md) before changing the planner or executor. Add regression coverage for behavior changes.

For editor highlighting, associate `*.coopr` with KDL and use a KDL v2 extension. In VS Code, add `"files.associations": { "*.coopr": "kdl" }` to your settings. Coopr instruction names are lowercase; command scripts use ordinary KDL multiline strings.

## Build from source

Build the CLI with Nix:

```sh
nix build path:.#coopr -o result-coopr
./result-coopr/bin/coopr --version
```

`path:.` includes untracked files. A [Git-based flake](https://nix.dev/tutorials/working-with-local-files.html#matching-files-tracked-by-git), such as `nix build .#coopr`, uses tracked files with their current working-tree contents.

## Check changes

Use the focused commands while iterating, then run the full flake checks:

| Command | Checks |
| --- | --- |
| `just test` | Unit tests using Go's `-short` mode. |
| `just vet` / `just lint` | Go static analysis. |
| `just scan` | Semgrep security checks for Go, Python, and GitHub Actions. |
| `just fmt` | Format Go, Nix, and the Justfile in place. |
| `just check` | Full flake checks, including untracked files. On amd64, these include the KVM suites below. |
| `just release-check` | Full flake checks using tracked files, including uncommitted changes. |

GitHub Actions runs quality and documentation checks once on amd64, native build checks on amd64 and arm64, and rootless tests in isolated amd64 VMs. `nix/ci.nix` assigns checks to these categories and generates their matrices through `nix-github-actions`. Register new checks in the relevant category.

Semgrep and CodeQL run separately. Semgrep uses pinned upstream rules and reports findings to GitHub code scanning; configuration and scanner errors fail its job.

## Edit documentation

```sh
just docs-serve
just docs-check
```

Open the preview at <http://127.0.0.1:8000>. `docs-check` builds the site strictly, parses public KDL examples, and tests the installer with local fixtures. `just docs-build` puts the generated site in `result-docs/` and verifies that the installer reaches the site unchanged.

Styling, templates, copy controls, and highlighting come from the pinned [Strio Zensical theme](https://github.com/strio-dev/strio-zensical-theme). Its Nix helpers supply the shared files during builds and previews. Coopr keeps its content, navigation, logo, and favicon in this repository.

Add pages to `zensical.toml`. Keep tutorials runnable from a fresh checkout and use the existing syntax and assets.

## Run integration checks

`just integration` and `just release-acceptance` require an AMD64 host with KVM. Isolated NixOS VMs run live integration tests and both dynamic and static packaged CLI acceptance as a normal user, including foreign-architecture execution.

Nix compiles the integration tests once. `nix/tests/rootless.nix` groups the Buildah tests by subsystem and defines the remaining VM checks. Its runner verifies test-group coverage and completion.

Live checks omit `-short`; external suites use the build tags selected by their Nix shells. A selected live suite fails when prerequisites are missing. Packaged acceptance uses the same Go suite in a VM or on a configured host.

For checks on your host, configure [rootless containers](guides/security.md) first. These commands use Podman rather than setting up a VM:

| Command | Scope |
| --- | --- |
| `just packaged-acceptance` | Packaged image, store reuse, multi-platform components, and Podman comparison. |
| `just docker-acceptance` | Docker transfers using a disposable daemon started through Podman; no existing Docker daemon is needed. |
| `just examples-acceptance` | Publish and consume example components through a temporary local registry, then run the image. Requires network access for upstream inputs, not a registry account. |
| `just benchmark` | Coopr/Podman timing measurements. |

`just cosign-acceptance` verifies a registry signature with an external `cosign` executable on `PATH`. Skipped integration tests do not establish runtime support.

## Releases

After checks pass, push an unprefixed version tag such as `1.2.3` or `1.2.3-rc.1`. CI takes the build version from that tag and publishes Linux binary archives, corresponding sources, checksums, and a multi-platform GHCR image. No manual version bump is required.

Prereleases keep their versioned tags; only the latest stable release updates the container's `latest` alias. Container tags replace `+` with `_` for build metadata, while the binary retains the exact release version. Documentation deploys from `main` after its checks pass.

### Corresponding source

Each release provides `coopr-sources.tar.gz` and a matching `ghcr.io/strio-dev/coopr:source-<version>` image containing the same source bundle. The runtime image's `org.opencontainers.image.source` label links to that release's `coopr-sources.tar.gz` asset. Source images are extracted, not run.

With [regctl](https://regclient.org/cli/regctl/image/get-file/), extract a release's source bundle directly from GHCR:

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
