{
  description = "Coopr OCI container composition tool";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-parts.url = "github:hercules-ci/flake-parts";
    flake-parts.inputs.nixpkgs-lib.follows = "nixpkgs";
    nix-github-actions.url = "github:nix-community/nix-github-actions";
    nix-github-actions.inputs.nixpkgs.follows = "nixpkgs";
    nix2container.url = "github:nlewo/nix2container/42d1df94f001a9416ef08be7b305144074aca9d2";
    nix2container.inputs.nixpkgs.follows = "nixpkgs";
    strio-theme.url = "github:strio-dev/strio-zensical-theme";
    strio-theme.inputs.nixpkgs.follows = "nixpkgs";
    strio-theme.inputs.flake-parts.follows = "flake-parts";
  };

  outputs =
    inputs:
    inputs.flake-parts.lib.mkFlake { inherit inputs; } {
      imports = [
        ./nix/ci.nix
      ];
      systems = [
        "x86_64-linux"
        "aarch64-linux"
      ];
      flake.overlays.default = import ./nix/overlay.nix {
        inherit (inputs) nix2container;
      };
      perSystem =
        {
          config,
          pkgs,
          system,
          ...
        }:
        let
          version =
            if builtins.pathExists ./nix/release-version then
              pkgs.lib.fileContents ./nix/release-version
            else
              "dev";
          sourceUrl =
            if version == "dev" then
              "https://github.com/strio-dev/coopr"
            else
              "https://github.com/strio-dev/coopr/releases/download/${version}/coopr-sources.tar.gz";
          nix2container = (import inputs.nix2container { inherit pkgs; }).nix2container;
          coopr = (pkgs.extend inputs.self.overlays.default).coopr;
          podman-sequoia = pkgs.callPackage ./nix/podman-sequoia.nix { };
          coopr-sequoia-signing = pkgs.callPackage ./nix/sequoia-helper.nix {
            inherit coopr podman-sequoia;
          };
          coopr-static = pkgs.callPackage ./nix/package-static.nix { inherit pkgs version; };
          nix-exporter = pkgs.callPackage ./nix/exporter.nix { };
          release-sources = pkgs.callPackage ./nix/release-sources.nix {
            inherit
              pkgs
              inputs
              coopr-static
              container
              ;
          };
          release = import ./nix/release-artifacts.nix {
            inherit
              pkgs
              coopr-static
              release-sources
              nix2container
              sourceUrl
              ;
          };
          zensical = inputs.strio-theme.packages.${system}.zensical;
          docs = pkgs.callPackage ./nix/docs.nix {
            inherit system;
            inherit (inputs) strio-theme;
          };
          container = pkgs.callPackage ./nix/container.nix {
            inherit coopr nix2container sourceUrl;
          };
          container-archive = pkgs.callPackage ./nix/container-archive.nix { inherit container; };
        in
        {
          packages = {
            default = coopr;
            inherit
              coopr
              coopr-sequoia-signing
              coopr-static
              nix-exporter
              docs
              container
              container-archive
              release-sources
              ;
            release-binary = release.binary;
            release-source = release.sources;
            container-source = release.sourceImage;
            coopr-sequoia-signing-sources = coopr-sequoia-signing.sources;
            nix-environment = import ./examples/nix/image.nix {
              pkgs = pkgs.extend inputs.self.overlays.default;
            };
            nix-tools-component = import ./examples/nix/component.nix {
              pkgs = pkgs.extend inputs.self.overlays.default;
            };
          };
          apps = {
            default = config.apps.coopr;
            docs-preview = inputs.strio-theme.lib.mkPreview {
              inherit system;
              sourcePaths = [
                "docs"
                "zensical.toml"
              ];
            };
            coopr = {
              type = "app";
              program = "${coopr}/bin/coopr";
            };
          };
          checks =
            (import ./nix/checks.nix {
              inherit
                pkgs
                coopr
                coopr-sequoia-signing
                coopr-static
                container
                docs
                release
                container-archive
                sourceUrl
                ;
              inherit (pkgs) lib;
            })
            // {
              inherit nix-exporter;
              nix-component = import ./nix/tests/nix-component.nix { inherit pkgs; };
              nix-image = import ./nix/tests/nix-image.nix {
                inherit pkgs;
                inherit (coopr) buildImage;
              };
              nix-composition = import ./nix/tests/nix-composition.nix { inherit pkgs; };
            };
          formatter = pkgs.nixfmt-tree;
          devShells.default = pkgs.callPackage ./nix/devshell.nix { inherit coopr zensical; };
          devShells.runtime =
            (pkgs.callPackage ./nix/devshell.nix {
              inherit coopr;
              withDevelopmentTools = false;
            }).overrideAttrs
              {
                COOPR_TEST_FIXTURES = pkgs.callPackage ./nix/tests/fixtures.nix { inherit coopr; };
              };
          devShells.api-service =
            (pkgs.callPackage ./nix/devshell.nix {
              inherit coopr;
              withDevelopmentTools = false;
            }).overrideAttrs
              {
                COOPR_TEST_SERVICE_BINARY = "${coopr}/bin/coopr";
              };
          devShells.composition =
            (pkgs.callPackage ./nix/devshell.nix {
              inherit coopr;
              withDevelopmentTools = false;
            }).overrideAttrs
              (old: {
                nativeBuildInputs = old.nativeBuildInputs ++ [ pkgs.python3 ];
              });
          devShells.examples = pkgs.callPackage ./nix/devshell.nix {
            inherit coopr;
            withDevelopmentTools = false;
            testTags = [ "examplesintegration" ];
          };
          devShells.docker = pkgs.callPackage ./nix/devshell.nix {
            inherit coopr;
            withDevelopmentTools = false;
            testTags = [ "dockerintegration" ];
          };
          devShells.cosign = pkgs.callPackage ./nix/devshell.nix {
            inherit coopr;
            withDevelopmentTools = false;
            testTags = [ "cosignintegration" ];
          };
          devShells.release = pkgs.callPackage ./nix/release.nix { };
          devShells.security = pkgs.callPackage ./nix/security.nix { };
        };
    };
}
