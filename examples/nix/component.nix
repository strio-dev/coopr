{ pkgs }:
pkgs.coopr.buildComponent {
  name = "coopr-nix-tools";
  tag = "dev";
  packages = [
    pkgs.bash
    pkgs.coreutils
    pkgs.hello
  ];
}
