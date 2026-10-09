{
  description = "Coopr OCI container composition tool";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-parts.url = "github:hercules-ci/flake-parts";
    flake-parts.inputs.nixpkgs-lib.follows = "nixpkgs";
    nix-github-actions.url = "github:nix-community/nix-github-actions";
    nix-github-actions.inputs.nixpkgs.follows = "nixpkgs";
    nix2container.url = "github:nlewo/nix2container/76be9608a7f4d6c985d28b0e7be903ae2547df3e";
    nix2container.inputs.nixpkgs.follows = "nixpkgs";
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
      perSystem =
        { config, pkgs, ... }:
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
          coopr = pkgs.callPackage ./nix/package.nix { inherit version; };
          coopr-static = pkgs.callPackage ./nix/package-static.nix { inherit pkgs version; };
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
          zensical = pkgs.callPackage ./nix/zensical.nix { };
          docs = pkgs.callPackage ./nix/docs.nix { inherit zensical; };
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
              coopr-static
              docs
              container
              container-archive
              release-sources
              ;
            release-binary = release.binary;
            release-source = release.sources;
            container-source = release.sourceImage;
          };
          apps = {
            default = config.apps.coopr;
            coopr = {
              type = "app";
              program = "${coopr}/bin/coopr";
            };
          };
          checks = import ./nix/checks.nix {
            inherit
              pkgs
              coopr
              coopr-static
              container
              docs
              release
              container-archive
              sourceUrl
              ;
            inherit (pkgs) lib;
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
