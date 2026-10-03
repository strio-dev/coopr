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
          coopr = pkgs.callPackage ./nix/package.nix { };
          zensical = pkgs.callPackage ./nix/zensical.nix { };
          docs = pkgs.callPackage ./nix/docs.nix { inherit zensical; };
          container = pkgs.callPackage ./nix/container.nix {
            inherit coopr;
            nix2container = (import inputs.nix2container { inherit pkgs; }).nix2container;
          };
        in
        {
          packages = {
            default = coopr;
            inherit coopr docs container;
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
              docs
              container
              ;
            inherit (pkgs) lib;
          };
          formatter = pkgs.nixfmt-tree;
          devShells.default = pkgs.callPackage ./nix/devshell.nix { inherit coopr zensical; };
        };
    };
}
