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

`docs-check` runs a strict Zensical build and parses the public KDL examples with the existing Go test. Zensical validates Markdown links and anchors.

Keep pages task-oriented and examples grounded in supported syntax. Add new pages to `zensical.toml`. Use system body fonts and preserve the accepted logo outlines. Local preview does not publish anything.

## Runtime checks

`just integration` enables the live Buildah, registry, and native-storage tests on a configured Linux host. `just packaged-acceptance` checks the self-contained image, including offline store reuse, multi-platform components, and comparison with Podman builds. `just release-acceptance` runs both. Skipped tests must stay visible in private validation evidence.

## Continuous integration

GitHub Actions builds the Nix flake checks on pushes, pull requests, and manual dispatch. These cover tests, vet, lint, formatting, workflow validation, public KDL examples, and builds of the CLI, container, and documentation. Live rootless acceptance requires a configured Linux host and remains available through the runtime commands above.

`nix-github-actions` generates the CI matrix from the flake's checks and supported platforms. `nix/ci.nix` connects the checks to the generator, using its default GitHub runners; adding a flake check adds it to the matrix. CI does not publish images, releases, or the documentation site.
