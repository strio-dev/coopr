{ nix2container }:
final: _prev:
let
  version =
    if builtins.pathExists ./release-version then final.lib.fileContents ./release-version else "dev";
  package = final.callPackage ./package.nix { inherit version; };
  buildImage = import ./build-image.nix {
    pkgs = final;
    coopr = package;
    nix2container = (import nix2container { pkgs = final; }).nix2container;
  };
  buildComponent = import ./build-component.nix {
    pkgs = final;
    coopr = package;
    nix2container = (import nix2container { pkgs = final; }).nix2container;
  };
in
{
  coopr = package.overrideAttrs (old: {
    passthru = (old.passthru or { }) // {
      inherit buildImage buildComponent;
    };
  });
}
