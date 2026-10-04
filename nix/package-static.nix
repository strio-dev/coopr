{ pkgs }:
let
  gpgme = pkgs.pkgsStatic.gpgme.overrideAttrs (old: {
    # Let GPGME find the host's GnuPG engines through PATH.
    configureFlags = builtins.filter (
      flag: !(pkgs.lib.hasPrefix "--enable-fixed-path=" flag)
    ) old.configureFlags;
  });
  releaseLibraries = {
    inherit gpgme;
    gcc = pkgs.pkgsStatic.stdenv.cc.cc;
    inherit (pkgs.pkgsStatic)
      libassuan
      libgpg-error
      libseccomp
      musl
      ;
  };
  licenseFiles = {
    gcc = [
      "COPYING3"
      "COPYING.RUNTIME"
    ];
    gpgme = [
      "COPYING"
      "COPYING.LESSER"
      "AUTHORS"
      "LICENSES"
    ];
    libassuan = [
      "COPYING.LIB"
      "AUTHORS"
    ];
    libgpg-error = [
      "COPYING.LIB"
      "AUTHORS"
    ];
    libseccomp = [
      "LICENSE"
      "CREDITS"
    ];
    musl = [ "COPYRIGHT" ];
  };
  installLicenses = pkgs.lib.concatStringsSep "\n" (
    pkgs.lib.mapAttrsToList (
      name: files:
      let
        package = releaseLibraries.${name};
      in
      ''mkdir -p "$out/share/licenses/coopr/third-party/${name}"''
      + "\n"
      + pkgs.lib.concatMapStringsSep "\n" (file: ''
        if [ -d ${package.src} ]; then
          cp ${package.src}/${file} "$out/share/licenses/coopr/third-party/${name}/${file}"
        else
          tar -xOf ${package.src} ${name}-${package.version}/${file} > "$out/share/licenses/coopr/third-party/${name}/${file}"
        fi
        chmod 644 "$out/share/licenses/coopr/third-party/${name}/${file}"
      '') files
    ) licenseFiles
  );
in
(pkgs.pkgsStatic.callPackage ./package.nix {
  inherit gpgme;
  inherit (pkgs) go_1_27;
}).overrideAttrs
  (
    final: old:
    let
      vendorSource = if final.vendorHash == null then "${final.src}/vendor" else final.goModules;
    in
    {
      pname = "coopr-static";
      ldflags = (old.ldflags or [ ]) ++ [
        "-linkmode=external"
        "-extldflags=-static"
      ];
      nativeBuildInputs = (old.nativeBuildInputs or [ ]) ++ [
        pkgs.buildPackages.gnutar
        pkgs.buildPackages.bzip2
        pkgs.buildPackages.gzip
        pkgs.buildPackages.xz
      ];
      passthru = (old.passthru or { }) // {
        inherit releaseLibraries;
      };
      postInstall =
        (old.postInstall or "")
        + installLicenses
        + ''
          mkdir -p "$out/share/licenses/coopr/third-party/go"
          for file in LICENSE PATENTS; do
            tar -xOf ${pkgs.go_1_27.src} "go/$file" > "$out/share/licenses/coopr/third-party/go/$file"
          done
          while IFS= read -r -d "" file; do
            relative="''${file#${vendorSource}/}"
            install -Dm644 "$file" "$out/share/licenses/coopr/third-party/go-modules/$relative"
          done < <(find ${vendorSource} -type f \( \
            -iname 'license*' -o -iname 'licence*' -o -iname 'copying*' \
            -o -iname 'notice*' -o -iname 'copyright*' -o -iname 'patents*' \
            -o -iname 'authors*' -o -iname 'unlicense*' \) ! -iname '*.go' ! -iname '*.proto' -print0)
          cat > "$out/share/licenses/coopr/third-party/NOTICE" <<'NOTICE'
          Coopr statically links GPGME, libassuan, libgpg-error, libseccomp, and musl.
          Their licenses and copyright notices accompany this binary. GPGME,
          libassuan, libgpg-error, and libseccomp are covered by the GNU LGPL.
          Go runtime and vendored Go dependency notices are also included. GCC
          startup code is covered by the GCC Runtime Library Exception included here.

          The corresponding source archive provided with the same release contains
          the library sources, build definitions, patches, and rebuild instructions.
          Use those materials to rebuild Coopr with modified versions of the libraries.
          No additional restrictions apply to modifying the libraries or reverse
          engineering Coopr to debug those modifications.
          NOTICE
        '';
    }
  )
