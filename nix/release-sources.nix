{
  pkgs,
  inputs,
  coopr-static,
  container,
}:
let
  inherit (pkgs) lib;
  nixExporter = pkgs.callPackage ./exporter.nix { };
  rootFiles = builtins.readDir ../.;
  rootGo = map (name: ../. + "/${name}") (
    lib.filter (name: rootFiles.${name} == "regular" && lib.hasSuffix ".go" name) (
      builtins.attrNames rootFiles
    )
  );
  source = lib.fileset.toSource {
    root = ../.;
    fileset = lib.fileset.unions (
      rootGo
      ++ [
        ../.github
        ../cmd
        ../docs
        ../examples
        ../internal
        ../api
        ../client
        ../nix
        # Keep generated Python bytecode out of corresponding source without
        # deleting local caches or relying on Git's tracked-file selection.
        (lib.fileset.fileFilter (file: !(file.hasExt "pyc" || file.hasExt "pyo")) ../scripts)
        ../CONTRIBUTING.md
        ../Justfile
        ../LICENSE
        ../README.md
        ../flake.nix
        ../flake.lock
        ../go.mod
        ../go.sum
        ../zensical.toml
      ]
    );
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
          inputArchive =
            pkgs.runCommand "${name}-source.tar.gz"
              {
                nativeBuildInputs = [
                  pkgs.gnutar
                  pkgs.gzip
                ];
              }
              ''
                tar --mode=u+w -C ${inputSource} -czf "$out" .
              '';
        in
        ''
          cp ${inputArchive} "$out/build-inputs/${name}.tar.gz"
        ''
      )
      [
        "nixpkgs"
        "flake-parts"
        "nix-github-actions"
        "nix2container"
        "strio-theme"
      ]
  );
in
pkgs.runCommand "coopr-release-sources" { } ''
  mkdir -p "$out/build-inputs" "$out/sources"
  cp -r ${source} "$out/coopr"
  cp -r ${coopr-static.goModules} "$out/vendor"
  cp -r ${nixExporter.goModules} "$out/nix-exporter-vendor"
  cp -r ${container.licenseSources}/. "$out/"
  ${copyLibraries}
  ${copyInputs}
  substitute ${./REBUILD.txt.in} "$out/REBUILD.txt" \
    --replace-fail '@hostSystem@' ${lib.escapeShellArg pkgs.stdenv.hostPlatform.system} \
    --replace-fail '@gpgmeVersion@' ${lib.escapeShellArg coopr-static.releaseLibraries.gpgme.version}
''
