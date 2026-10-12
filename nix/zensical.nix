{
  lib,
  zensical,
  fetchPypi,
  rustPlatform,
  runCommand,
  makeWrapper,
  chroma,
}:
# 0.0.63 excludes docs/.overrides from the generated site.
let
  upstream = zensical.overrideAttrs (
    finalAttrs: _previousAttrs: {
      version = "0.0.63";
      src = fetchPypi {
        inherit (finalAttrs) pname version;
        hash = "sha256-lfaLSU+moRpZBl95ZdJ6z8pATdhn+B0xKVtatxcvL4o=";
      };
      cargoDeps = rustPlatform.fetchCargoVendor {
        inherit (finalAttrs) pname version src;
        hash = "sha256-4PTQVZbEjChCyRpNkzvL7jahobehSUc6xMFeRhpdAsA=";
      };
    }
  );
in
runCommand "zensical-coopr" { nativeBuildInputs = [ makeWrapper ]; } ''
  mkdir -p "$out/bin"
  makeWrapper ${upstream}/bin/zensical "$out/bin/zensical" \
    --prefix PATH : ${lib.makeBinPath [ chroma ]} \
    --prefix PYTHONPATH : ${../docs/.overrides}
''
