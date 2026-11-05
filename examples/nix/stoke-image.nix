{ pkgs, stokeSource }:
let
  go = pkgs.callPackage (stokeSource + "/nix/go.nix") { };
  stoke = pkgs.callPackage (stokeSource + "/nix/package.nix") { inherit go; };
  sourceRoot = pkgs.runCommand "coopr-stoke-source-and-vendor" { } ''
    mkdir -p "$out/workspace" "$out/tmp" "$out/cache/build" "$out/cache/tests"
    cp -r ${stoke.src}/. "$out/workspace/"
    cp -r ${stoke.goModules} "$out/workspace/vendor"
    chmod -R u+w "$out/workspace"
    chmod 1777 "$out/tmp"
  '';
in
pkgs.coopr.buildImage {
  name = "coopr-nix-stoke";
  tag = "dev";
  packages = [
    pkgs.bash
    pkgs.coreutils
    pkgs.gitMinimal
  ];
  copyToRoot = [ sourceRoot ];
  definition = ./go-image.coopr;
  context = ./.;
  # The Go closure enters the image through the argument's Nix string context.
  # It is deliberately not repeated in packages or initial image configuration.
  args.go = "${go}/bin/go";
  config = {
    Env = [
      "PATH=/bin"
      "COOPR_ENVIRONMENT=nix-stoke"
    ];
    WorkingDir = "/workspace";
  };
}
