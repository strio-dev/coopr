# Contributing

Use the pinned development shell and keep changes focused:

```sh
nix develop path:.
coopr --help
just test
just vet
just lint
just check
```

The default shell (also loaded by direnv) puts the Nix-built `coopr` package on `PATH`. To run source edits immediately, use Go from the repository root:

```sh
go run ./cmd/coopr --help
go run ./cmd/coopr build examples/first-image/image.coopr --tag coopr-demo:base
```

Re-enter the development shell after rebuilding to use the updated packaged CLI.

`nix run path:. -- --help` runs the distributable package. `nix build path:.#coopr` builds it. For focused validation, the flake provides `#checks.<system>.test`, `.vet`, `.lint`, `.docs` and `.build` on `x86_64-linux` and `aarch64-linux`. Package, container, check, and shell definitions live under `nix/`; `flake.nix` wires their outputs together.

The `path:.` source includes untracked files. `just release-check` uses the Git source, so it checks a different boundary while work is untracked. Add regression coverage for changed behavior and preserve ordered instructions, component phase separation, and image/component storage boundaries.

## Work on documentation

```sh
just docs-serve
just docs-check
```

`docs-check` runs a strict Zensical build, parses the public KDL examples with the existing Go test, and exercises the release installer with local fixtures. Zensical validates Markdown links and anchors. The Nix docs build also checks that `install.sh` is copied unchanged to the site root.

Keep pages task-oriented and examples grounded in supported syntax. Add new pages to `zensical.toml`. Use system body fonts and preserve the accepted logo outlines.

## Runtime checks

`just integration` enables the live Buildah, registry, and native-storage tests on a configured Linux host. `just packaged-acceptance` checks the self-contained image, including offline store reuse, multi-platform components, and comparison with Podman builds. `just release-acceptance` runs both. Skipped tests must stay visible in private validation evidence.

## Continuous integration

GitHub Actions builds the Nix flake checks on pushes, pull requests, and manual dispatch. These cover tests, vet, lint, formatting, workflow validation, public KDL examples, and builds of the CLI, container, documentation, and release archives. Native checks extract each binary archive, check its bundled notices, and run the static executable without a system library path. The source archive is checked once on amd64. Live rootless acceptance requires a configured Linux host and remains available through the runtime commands above.

`nix-github-actions` generates the CI matrix from the flake's checks and supported platforms. `nix/ci.nix` connects the checks to the generator, using its default GitHub runners. Checks are registered explicitly in the quality, documentation, or native categories in `nix/ci.nix`; adding a flake check also requires adding it to the appropriate category.

Release pushes use unprefixed version tags such as `1.2.3`, `1.2.3-rc.1`, and `1.2.3+build.1`. GitHub's native tag globs select these version shapes; they do not enforce all SemVer rules, such as leading-zero and identifier restrictions. Underscores are excluded so build metadata can use Helm's registry convention of replacing `+` with `_` without colliding with prerelease tags. Branch pushes and pull requests still run all CI categories.

Nix produces the downloadable binary and source archives. On a tag push, native release runners run the same checks and push containers directly with nix2container. Registry write access is confined to these tag jobs and the final publishing job. Only binary archives pass between jobs; regctl combines the registry images into a multi-platform index. Intermediate tags use `build-<run-id>-amd64` and `build-<run-id>-arm64`, so retrying a failed job can reuse the successful architecture's push. The GitHub release action uploads the archives and notices to a draft before publication.

Releases queue through GitHub's native concurrency queue, which holds up to 100 pending jobs. A rerun completes an unfinished `latest` container alias using the published version's digest, while preserving published release assets and versioned container tags. Only the current latest stable GitHub release updates that alias.
