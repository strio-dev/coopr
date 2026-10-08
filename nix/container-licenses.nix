{
  pkgs,
  roots,
  runtimeRoots,
  coopr,
}:
let
  inherit (pkgs) lib;
  candidates = map (entry: entry.package) (
    builtins.genericClosure {
      startSet = map (package: {
        key = package.drvPath;
        inherit package;
      }) roots;
      operator =
        entry:
        map
          (package: {
            key = package.drvPath;
            inherit package;
          })
          (
            lib.filter lib.isDerivation (
              (entry.package.buildInputs or [ ])
              ++ (entry.package.propagatedBuildInputs or [ ])
              ++ (entry.package.propagatedUserEnvPkgs or [ ])
              ++ (entry.package.runtimeDependencies or [ ])
            )
          );
    }
  );
  sourceInputs =
    package:
    lib.unique (
      lib.optional (package ? src && package.src != null) package.src
      ++ (package.srcs or [ ])
      ++ lib.optional (package ? cargoDeps && package.cargoDeps != null) package.cargoDeps
      ++ lib.optional (package ? kernelSrc) package.kernelSrc
    );
  # This package's installed files are the unmodified source data, rather than an archive.
  dnsSource = pkgs.runCommand "dns-root-data-source" { } ''
    mkdir -p "$out"
    cp ${pkgs.dns-root-data}/{root.hints,root.key,root.ds} "$out/"
    cp ${pkgs.path}/pkgs/by-name/dn/dns-root-data/package.nix "$out/package.nix"
  '';
  # The IANA data release omits a license file; the registry data has its own dedication.
  ianaSource = pkgs.runCommand "iana-etc-source" { } ''
    cp -r ${pkgs.iana-etc.src} "$out"
    chmod u+w "$out"
    cp ${
      pkgs.fetchurl {
        url = "https://creativecommons.org/publicdomain/zero/1.0/legalcode.txt";
        hash = "sha256-ogEPNDSH0/dhiv/lT3ifVIdgIzHAqNA/SemnxUfPBJk=";
      }
    } "$out/LICENSE-CC0"
    install -Dm644 ${./notices/iana-etc.txt} "$out/NOTICE"
  '';
  packages = map (package: {
    name = "${package.pname or package.name}-${package.version or "unknown"}";
    outputs = map (
      output: builtins.unsafeDiscardStringContext (toString package.${output})
    ) package.outputs;
    sources =
      if package.drvPath == pkgs.dns-root-data.drvPath then
        [ dnsSource ]
      else if package.drvPath == pkgs.iana-etc.drvPath then
        [ ianaSource ]
      else
        sourceInputs package;
    raw = package.drvPath == pkgs.cacert.drvPath;
    dns = package.drvPath == pkgs.dns-root-data.drvPath;
    # newuidmap/newgidmap are copied into the image, without the Shadow output.
    copied = package.drvPath == pkgs.shadow.drvPath;
    # Coopr's source/dependencies are packaged once at the release root. These
    # two Apache-2.0 programs and their permissively licensed Rust dependencies
    # need notices, not full source delivery. Keep their recursive notices above.
    publishSource =
      !builtins.elem package.drvPath [
        coopr.drvPath
        pkgs.aardvark-dns.drvPath
        pkgs.netavark.drvPath
      ];
    patches = package.patches or [ ];
  }) candidates;
  sourcePackages = lib.filter (package: package.sources != [ ]) packages;
  runtimeClosure = pkgs.closureInfo { rootPaths = runtimeRoots; };
  isRuntime =
    package:
    if package.copied then
      "true"
    else
      lib.concatMapStringsSep " || " (
        output: "grep -Fxq ${lib.escapeShellArg output} ${runtimeClosure}/store-paths"
      ) package.outputs;
  tools = with pkgs.buildPackages; [
    gnutar
    gzip
    bzip2
    xz
    lzip
    unzip
  ];
  licenseBundle = pkgs.stdenvNoCC.mkDerivation {
    name = "coopr-container-licenses";
    dontUnpack = true;
    nativeBuildInputs = tools;
    passAsFile = [ "licenseScript" ];
    installPhase = ''source "$licenseScriptPath"'';
    licenseScript = lib.concatMapStringsSep "\n" (package: ''
      if ${isRuntime package}; then
      mkdir -p "$out/share/licenses/coopr/container/${package.name}"
      ${lib.concatImapStringsSep "\n" (index: source: ''
        work=$(mktemp -d)
        cd "$work"
        ${
          if package.raw then
            ''
              # certdata.txt carries the NSS copyright and MPL notice in its header.
              cp ${source} "$out/share/licenses/coopr/container/${package.name}/certdata.txt"
            ''
          else
            ''
              unpackFile ${lib.escapeShellArg "${source}"}
              while IFS= read -r -d "" file; do
                install -Dm644 "$file" "$out/share/licenses/coopr/container/${package.name}/${toString index}/''${file#./}"
              done < <(find . -type f \( \
                -iname 'license*' -o -iname 'licence*' -o -iname 'copying*' \
                -o -iname 'notice*' -o -iname 'copyright*' -o -iname 'patents*' \
                -o -iname 'authors*' -o -iname 'unlicense*' \
                -o -ipath '*/licenses/*' \) ! -iname '*.go' ! -iname '*.proto' -print0)
            ''
        }
      '') package.sources}
      ${lib.optionalString package.dns ''
        install -Dm644 ${./notices/dns-root-data.txt} "$out/share/licenses/coopr/container/${package.name}/NOTICE"
      ''}
      fi
    '') sourcePackages;
  };
  licenseSources = pkgs.runCommand "coopr-container-license-sources" { } (
    lib.concatMapStringsSep "\n" (package: ''
      if ${isRuntime package}; then
      mkdir -p "$out/container/${package.name}" "$out/sources"
      ${lib.concatImapStringsSep "\n" (index: source: ''
        target="$out/sources/${builtins.baseNameOf (toString source)}"
        if [ ! -e "$target" ]; then cp -r ${lib.escapeShellArg "${source}"} "$target"; fi
        ln -sfn ../../sources/${builtins.baseNameOf (toString source)} "$out/container/${package.name}/${toString index}-${builtins.baseNameOf (toString source)}"
      '') package.sources}
      ${lib.optionalString (package.patches != [ ]) ''
        mkdir -p "$out/container/${package.name}/patches"
        : > "$out/container/${package.name}/patches/series"
      ''}
      ${lib.concatImapStringsSep "\n" (index: patch: ''
        cp -rL ${lib.escapeShellArg "${patch}"} "$out/container/${package.name}/patches/${toString index}-${builtins.baseNameOf (toString patch)}"
        printf '%s\n' ${lib.escapeShellArg "${toString index}-${builtins.baseNameOf (toString patch)}"} >> "$out/container/${package.name}/patches/series"
      '') package.patches}
      fi
    '') (lib.filter (package: package.publishSource) sourcePackages)
  );
in
{
  inherit
    licenseBundle
    licenseSources
    packages
    runtimeClosure
    ;
  candidateOutputs = lib.concatMap (package: package.outputs) sourcePackages;
}
