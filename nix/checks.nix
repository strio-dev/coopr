{
  lib,
  pkgs,
  coopr,
  coopr-static,
  container,
  docs,
  container-archive,
  release,
  sourceUrl,
}:
let
  goCheck =
    name: command: inputs:
    coopr.overrideAttrs {
      name = "coopr-${name}";
      src = coopr.testSource;
      goModules = coopr.goModules;
      doCheck = true;
      nativeCheckInputs = [
        pkgs.gitMinimal
        pkgs.openssh
        pkgs.gnupg
      ]
      ++ inputs;
      buildPhase = "runHook preBuild; runHook postBuild";
      checkPhase = ''
        runHook preCheck
        export HOME="$TMPDIR"
        export XDG_DATA_HOME="$TMPDIR/xdg-data"
        export XDG_CONFIG_HOME="$TMPDIR/xdg-config"
        export GOLANGCI_LINT_CACHE="$TMPDIR/golangci-lint"
        mkdir -p "$XDG_DATA_HOME" "$XDG_CONFIG_HOME/containers"
        # Private test registries publish unsigned fixtures.
        cat > "$XDG_CONFIG_HOME/containers/policy.json" <<'EOF'
        {"default":[{"type":"insecureAcceptAnything"}]}
        EOF
        ${command}
        runHook postCheck
      '';
      env = {
        inherit (coopr) CGO_ENABLED;
        GOFLAGS = "-tags=${lib.concatStringsSep "," coopr.tags}";
      };
      installPhase = ''touch "$out"'';
      postInstall = "";
    };
  goTests = goCheck "test" "go test ./..." [ ];
  nativeGoCheck = if pkgs.stdenv.hostPlatform.isx86_64 then goChecks else goTests;
  goChecks = goCheck "go-check" ''
    go test ./...
    go vet ./...
    golangci-lint run --build-tags=${lib.concatStringsSep "," coopr.tags}
  '' [ pkgs.golangci-lint ];
  rootlessChecks = lib.optionalAttrs pkgs.stdenv.hostPlatform.isx86_64 (
    import ./tests/rootless.nix {
      inherit
        lib
        pkgs
        coopr
        coopr-static
        container
        ;
    }
  );
in
{
  build = coopr;
  go-check = nativeGoCheck;
  test = nativeGoCheck;
  vet = if pkgs.stdenv.hostPlatform.isx86_64 then goChecks else goCheck "vet" "go vet ./..." [ ];
  lint =
    if pkgs.stdenv.hostPlatform.isx86_64 then
      goChecks
    else
      goCheck "lint" "golangci-lint run --build-tags=${lib.concatStringsSep "," coopr.tags}" [
        pkgs.golangci-lint
      ];
  inherit docs;
  installer = pkgs.callPackage ./installer-check.nix { };
  container =
    pkgs.runCommand "coopr-container-license-check"
      {
        nativeBuildInputs = [ pkgs.jq ];
      }
      ''
        test -s ${container-archive}
        jq -e --arg source ${lib.escapeShellArg sourceUrl} \
          '."image-config".Labels["org.opencontainers.image.source"] == $source' ${container}
        test -s ${coopr}/share/licenses/coopr/third-party/go/LICENSE
        test -s ${coopr}/share/licenses/coopr/third-party/go-modules/go.podman.io/buildah/LICENSE
        jq -r '.[]' ${
          pkgs.writeText "coopr-license-outputs.json" (
            builtins.toJSON (container.candidateOutputs ++ container.knownGeneratedOutputs)
          )
        } | sort -u > attributed
        sort -u ${pkgs.closureInfo { rootPaths = container.runtimeRoots; }}/store-paths > shipped
        if grep -E '/[a-z0-9]+-libkrun(fw)?(-|$)' shipped > vm-runtime; then
          echo 'Container unexpectedly ships optional VM runtime dependencies:' >&2
          cat vm-runtime >&2
          exit 1
        fi
        comm -23 shipped attributed > missing
        if [ -s missing ]; then
          echo 'Container outputs missing corresponding license/source attribution:' >&2
          cat missing >&2
          exit 1
        fi
        for package in ${container.licenseBundle}/share/licenses/coopr/container/*; do
          if [ -z "$(find "$package" -type f -size +0c -print -quit)" ]; then
            echo "Container package has no readable license notice: $package" >&2
            exit 1
          fi
        done
        touch "$out"
      '';
  release-binary = release.check;
  release-source = release.sourceCheck;
  docs-examples =
    (goCheck "docs-examples"
      "COOPR_TEST_DOCS=1 go test -count=1 ./internal/definition -run '^TestDocumentationKDLExamples$'"
      [ ]
    ).overrideAttrs
      (oldAttrs: {
        postPatch = (oldAttrs.postPatch or "") + ''
          cp -r ${../docs} docs
          cp ${../README.md} README.md
          cp ${../CONTRIBUTING.md} CONTRIBUTING.md
        '';
      });
  workflow =
    pkgs.runCommand "coopr-workflow-check"
      {
        nativeBuildInputs = [
          pkgs.actionlint
          pkgs.shellcheck
        ];
      }
      ''
        # actionlint 1.7.12 does not yet recognize GitHub's native queue field.
        # Remove this exact diagnostic exception when upstream supports it:
        # https://github.com/rhysd/actionlint/issues/680
        actionlint -ignore '^unexpected key "queue" for "concurrency" section\. expected one of "cancel-in-progress", "group"$' \
          ${../.github/workflows}/*.yml
        touch "$out"
      '';
  justfile = pkgs.runCommand "coopr-justfile-check" { nativeBuildInputs = [ pkgs.just ]; } ''
    just --justfile ${../Justfile} --fmt --check
    touch "$out"
  '';
  scripts = pkgs.runCommand "coopr-scripts-check" { nativeBuildInputs = [ pkgs.shellcheck ]; } ''
    shellcheck \
      ${../scripts/acceptance/release.sh} \
      ${../scripts/acceptance/docker.sh} \
      ${../scripts/benchmarks/run.sh} \
      ${../docs/install.sh}
    touch "$out"
  '';
  formatting =
    pkgs.runCommand "coopr-formatting"
      {
        src = coopr.testSource;
        nativeBuildInputs = [
          pkgs.go_1_27
          pkgs.nixfmt
        ];
      }
      ''
        cp -r "$src" source
        chmod -R u+w source
        unformatted="$(gofmt -l source ${../scripts})"
        if [ -n "$unformatted" ]; then
          echo "Go files need gofmt:" >&2
          echo "$unformatted" >&2
          exit 1
        fi
        nixfmt --check ${../flake.nix} ${./.}/*.nix ${./tests}/*.nix
        touch "$out"
      '';
}
// rootlessChecks
// lib.optionalAttrs pkgs.stdenv.hostPlatform.isx86_64 {
  rootless = pkgs.linkFarm "coopr-rootless" (
    lib.mapAttrsToList (name: path: { inherit name path; }) rootlessChecks
  );
}
