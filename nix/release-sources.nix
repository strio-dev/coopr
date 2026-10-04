{
  pkgs,
  inputs,
  coopr-static,
}:
let
  inherit (pkgs) lib;
  source = lib.fileset.toSource {
    root = ../.;
    fileset = lib.fileset.unions [
      ../.github
      ../cmd
      ../docs
      ../examples
      ../internal
      ../nix
      ../scripts
      ../CONTRIBUTING.md
      ../Justfile
      ../LICENSE
      ../README.md
      ../flake.nix
      ../flake.lock
      ../go.mod
      ../go.sum
      ../zensical.toml
    ];
  };
  copyLibraries = lib.concatStringsSep "\n" (
    lib.mapAttrsToList (name: library: ''
      mkdir -p "$out/libraries/${name}"
      cp ${library.src} "$out/libraries/${name}/${library.src.name}"
    '') coopr-static.releaseLibraries
  );
  copyInputs = lib.concatStringsSep "\n" (
    map
      (name: ''
        tar -C ${inputs.${name}.outPath} -czf "$out/build-inputs/${name}.tar.gz" .
      '')
      [
        "nixpkgs"
        "flake-parts"
        "nix-github-actions"
        "nix2container"
      ]
  );
in
pkgs.runCommand "coopr-release-sources"
  {
    nativeBuildInputs = [
      pkgs.gnutar
      pkgs.gzip
    ];
  }
  ''
    mkdir -p "$out/build-inputs" "$out/go"
    cp -r ${source} "$out/coopr"
    cp -r ${coopr-static.goModules} "$out/vendor"
    cp ${pkgs.go_1_27.src} "$out/go/${pkgs.go_1_27.src.name}"
    ${copyLibraries}
    ${copyInputs}
    cat > "$out/REBUILD.txt" <<'REBUILD'
    Coopr corresponding source

    This archive accompanies the static Linux binaries in the same release.
    coopr/ contains the application source and its build definitions.
    vendor/ contains the Go dependencies used for that build, including their
    license files. go/ contains the Go compiler/runtime upstream source.
    libraries/ contains the exact upstream sources for GPGME, libassuan,
    libgpg-error, libseccomp, musl and the GCC compiler/runtime.
    build-inputs/ contains the pinned Nix
    inputs, including Nixpkgs' library patches and configuration recipes.

    With Nix installed, rebuild the original application from this directory:

      nix build path:./coopr#coopr-static
      ./result/bin/coopr --help

    Nix fetches the versions recorded in coopr/flake.lock and go.mod/go.sum.
    It may need network access to obtain the compiler and other build tools.

    To rebuild with a modified library, unpack its archive, make your changes,
    and override that library's src in coopr/nix/package-static.nix. For example,
    extract libraries/gpgme/*.tar.bz2 inside coopr/ and add this attribute to
    the existing gpgme.overrideAttrs block, using your extracted directory name:

      src = ../gpgme-${coopr-static.releaseLibraries.gpgme.version};

    Re-run the nix build command. Nix rebuilds GPGME and relinks Coopr against
    the modified library. Other linked libraries can likewise be overridden
    through pkgs.pkgsStatic.callPackage's arguments or its stdenv for musl.

    To use the bundled Go dependency sources, copy vendor/ to coopr/vendor/,
    include ../vendor in the fileset in coopr/nix/package.nix, and set
    vendorHash = null in that file. Go then builds with those vendored sources.

    The original Nix input source trees can be extracted from build-inputs/.
    Use nix build's --override-input option to select modified local input
    trees instead of the locked remote inputs, such as a modified Nixpkgs.

    Coopr's MIT license does not replace the licenses of its dependencies.
    See the notices shipped with the binary and the licenses in these sources.
    No additional restrictions apply to modifying the LGPL libraries or reverse
    engineering Coopr to debug those modifications.
    REBUILD
  ''
