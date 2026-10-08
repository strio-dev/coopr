{
  pkgs,
  inputs,
  coopr-static,
  container,
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
      target="$out/sources/${builtins.baseNameOf (toString library.src)}"
      if [ ! -e "$target" ]; then cp -r ${library.src} "$target"; fi
      ln -s ../../sources/${builtins.baseNameOf (toString library.src)} "$out/libraries/${name}/${library.src.name}"
      ${lib.optionalString ((library.patches or [ ]) != [ ]) ''
        mkdir -p "$out/libraries/${name}/patches"
        : > "$out/libraries/${name}/patches/series"
      ''}
      ${lib.concatImapStringsSep "\n" (index: patch: ''
        cp -rL ${lib.escapeShellArg "${patch}"} "$out/libraries/${name}/patches/${toString index}-${builtins.baseNameOf (toString patch)}"
        printf '%s\n' ${lib.escapeShellArg "${toString index}-${builtins.baseNameOf (toString patch)}"} >> "$out/libraries/${name}/patches/series"
      '') (library.patches or [ ])}
    '') coopr-static.releaseLibraries
  );
  copyInputs = lib.concatStringsSep "\n" (
    map
      (
        name:
        let
          inputSource = builtins.path {
            path = inputs.${name}.outPath;
            name = "${name}-source";
          };
        in
        ''
          tar --mode=u+w -C ${inputSource} -czf "$out/build-inputs/${name}.tar.gz" .
        ''
      )
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
    mkdir -p "$out/build-inputs" "$out/sources"
    cp -r ${source} "$out/coopr"
    cp -r ${coopr-static.goModules} "$out/vendor"
    cp -r ${container.licenseSources}/. "$out/"
    ${copyLibraries}
    ${copyInputs}
    substitute ${./REBUILD.txt.in} "$out/REBUILD.txt" \
      --replace-fail '@hostSystem@' ${lib.escapeShellArg pkgs.stdenv.hostPlatform.system} \
      --replace-fail '@gpgmeVersion@' ${lib.escapeShellArg coopr-static.releaseLibraries.gpgme.version}
  ''
