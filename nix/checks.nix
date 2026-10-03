{
  lib,
  pkgs,
  coopr,
  docs,
  container,
}:
let
  goCheck =
    name: command: inputs:
    coopr.overrideAttrs {
      name = "coopr-${name}";
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
in
{
  build = coopr;
  test = goCheck "test" "go test ./..." [ ];
  vet = goCheck "vet" "go vet ./..." [ ];
  lint = goCheck "lint" "golangci-lint run --build-tags=${lib.concatStringsSep "," coopr.tags}" [
    pkgs.golangci-lint
  ];
  inherit docs container;
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
        actionlint ${../.github/workflows/ci.yml}
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
      ${../scripts/benchmarks/run.sh}
    touch "$out"
  '';
  formatting =
    pkgs.runCommand "coopr-formatting"
      {
        src = coopr.src;
        nativeBuildInputs = [
          pkgs.go_1_27
          pkgs.nixfmt
        ];
      }
      ''
        cp -r "$src" source
        chmod -R u+w source
        unformatted="$(gofmt -l source)"
        if [ -n "$unformatted" ]; then
          echo "Go files need gofmt:" >&2
          echo "$unformatted" >&2
          exit 1
        fi
        nixfmt --check ${../flake.nix} ${./.}/*.nix
        touch "$out"
      '';
}
