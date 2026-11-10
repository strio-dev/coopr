{ pkgs }:
pkgs.coopr.buildImage {
  name = "coopr-nix-environment";
  tag = "dev";
  packages = [
    pkgs.bash
    pkgs.coreutils
  ];
  definition = ./environment.coopr;
  context = ./.;
  args.message = "nix-and-coopr";
  config = {
    Entrypoint = [ "${pkgs.bash}/bin/bash" ];
    Env = [
      "PATH=/bin"
      "COOPR_ENVIRONMENT=nix"
    ];
    WorkingDir = "/";
  };
}
