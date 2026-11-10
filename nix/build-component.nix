{
  pkgs,
  nix2container,
  coopr,
  exporter ? pkgs.callPackage ./exporter.nix { },
}:
import ./build-image.nix {
  inherit
    pkgs
    nix2container
    coopr
    exporter
    ;
  artifact = "component";
}
