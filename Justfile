set default-list

# Run unit tests; integration tasks select the live suites.
test:
    nix develop path:. -c env TMPDIR=/tmp go test -short ./...

# Run Go static analysis in the Nix development environment.
vet:
    nix develop path:. -c go vet ./...

# Run the pinned Go linter in the Nix development environment.
lint:
    nix develop path:. -c golangci-lint run

# Run the pinned security rules without building Coopr.
scan:
    nix develop --no-update-lock-file path:.#security -c semgrep scan --no-error --strict --jobs 2 . nix/tests/installer.py

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

# Run rootless integration and packaged acceptance in AMD64 NixOS VMs with KVM.
integration:
    nix build --no-link --no-update-lock-file --print-build-logs path:.#checks.x86_64-linux.rootless

# Verify the complete AMD64 rootless release check.
release-acceptance: integration

# Run the raw packaged-image harness in an already configured rootless host.
packaged-acceptance:
    nix develop path:.#runtime -c ./scripts/acceptance/release.sh

# Compare current Coopr and Podman cold, warm, changed-step, and concurrent builds.
benchmark iterations="5" multiplatform="0":
    nix develop path:. -c ./scripts/benchmarks/run.sh {{ quote(iterations) }} {{ quote(multiplatform) }}

# Verify explicit Docker engine transfers and imports with a disposable daemon.
docker-acceptance store="modern":
    nix develop path:.#docker -c ./scripts/acceptance/docker.sh {{ quote(store) }}

# Publish the repository examples, remove publisher files, and run the consumer.
examples-acceptance:
    nix develop path:.#examples -c go test -count=1 -v -timeout=15m ./internal/acceptance -run '^TestPublishedExamplesSurvivePublisherRemovalAndRun$'

# Verify a registry signature with an installed Cosign CLI.
cosign-acceptance:
    nix develop path:.#cosign -c go test -count=1 -v ./internal/transfer -run '^TestCopyRootSigstoreSignatureWithCosign$'

# Build the local documentation site with the repository-pinned Zensical.
docs-build:
    nix develop path:. -c zensical build --clean --strict

# Preview documentation on localhost; this does not publish the site.
docs-serve:
    nix develop path:. -c zensical serve

# Build documentation strictly and parse its KDL examples.
docs-check: docs-build
    nix develop path:. -c go test -count=1 ./internal/definition -run TestDocumentationKDLExamples
    nix develop path:. -c python3 nix/tests/installer.py docs/install.sh
