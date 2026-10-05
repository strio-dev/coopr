set default-list

# Run Go tests; live tests skip unless their environment is configured.
test:
    nix develop path:. -c env TMPDIR=/tmp go test ./...

# Run Go static analysis in the Nix development environment.
vet:
    nix develop path:. -c go vet ./...

# Run the pinned Go linter in the Nix development environment.
lint:
    nix develop path:. -c golangci-lint run

# Format Go, Nix, and Justfile sources with pinned tools.
fmt:
    nix develop path:. -c gofmt -w cmd internal
    nix develop path:. -c nixfmt flake.nix nix/*.nix nix/tests/*.nix
    nix develop path:. -c just --fmt

# Run the complete local flake check, including untracked sources.
check:
    nix flake check path:. --no-update-lock-file

# Build the Coopr CLI.
build:
    nix build path:.#coopr --out-link result-coopr

# Build a loadable Coopr CLI container archive.
container:
    nix build path:.#container-archive --out-link result-container

# Check the exact Git source that would be released.
release-check:
    nix flake check . --no-update-lock-file

# Run rootless integration and dynamic/static packaged acceptance in NixOS.
integration:
    nix build --no-link --no-update-lock-file --print-build-logs path:.#checks.{{ arch() }}-linux.rootless

# Verify the complete native rootless release check.
release-acceptance: integration

# Run the raw packaged-image harness in an already configured rootless host.
packaged-acceptance:
    nix develop path:.#runtime -c ./scripts/acceptance/release.sh

# Compare current Coopr and Podman cold, warm, changed-step, and concurrent builds.
benchmark:
    ./scripts/benchmarks/run.sh

# Verify explicit Docker engine transfers and imports with a disposable daemon.
docker-acceptance:
    ./scripts/acceptance/docker.sh

# Build the local documentation site with the repository-pinned Zensical.
docs-build:
    nix develop path:. -c zensical build --clean --strict

# Preview documentation on localhost; this does not publish the site.
docs-serve:
    nix develop path:. -c zensical serve

# Build documentation strictly and parse its KDL examples.
docs-check: docs-build
    nix develop path:. -c env COOPR_TEST_DOCS=1 go test -count=1 ./internal/definition -run TestDocumentationKDLExamples
    nix develop path:. -c python3 nix/tests/installer.py docs/install.sh
