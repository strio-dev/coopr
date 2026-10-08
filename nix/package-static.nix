{
  pkgs,
  version ? "dev",
}:
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
  inherit gpgme version;
  inherit (pkgs) go_1_27;
}).overrideAttrs
  (
    final: old: {
      pname = "coopr-static";
      ldflags = (old.ldflags or [ ]) ++ [
        "-linkmode=external"
        "-extldflags=-static"
      ];
      nativeBuildInputs = (old.nativeBuildInputs or [ ]) ++ [
        pkgs.buildPackages.bzip2
        pkgs.buildPackages.xz
      ];
      passthru = (old.passthru or { }) // {
        inherit releaseLibraries;
      };
      postInstall =
        (old.postInstall or "")
        + installLicenses
        + ''
          install -Dm644 ${./notices/static.txt} "$out/share/licenses/coopr/third-party/NOTICE"
        '';
    }
  )
