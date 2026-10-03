{
  zensical,
  fetchPypi,
  rustPlatform,
}:
# 0.0.63 excludes docs/.overrides from the generated site.
zensical.overrideAttrs (
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
)
