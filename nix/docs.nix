{
  lib,
  strio-theme,
  system,
}:
let
  site = strio-theme.lib.mkZensicalSite {
    inherit system;
    name = "coopr-docs";
    src = lib.fileset.toSource {
      root = ../.;
      fileset = lib.fileset.unions [
        ../docs
        ../zensical.toml
      ];
    };
  };
in
site.overrideAttrs (old: {
  buildCommand = old.buildCommand + ''
    test ! -e "$out/.overrides"
    cmp ${../docs/install.sh} "$out/install.sh"
  '';
})
