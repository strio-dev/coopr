{
  pkgs,
  coopr-static,
  release-sources,
}:
let
  arch =
    {
      x86_64-linux = "amd64";
      aarch64-linux = "arm64";
    }
    .${pkgs.stdenv.hostPlatform.system};
  tarFlags = "--sort=name --mtime=@1 --owner=0 --group=0 --numeric-owner";
  binary =
    pkgs.runCommand "coopr-linux-${arch}.tar.gz"
      {
        nativeBuildInputs = [
          pkgs.gnutar
          pkgs.gzip
        ];
      }
      ''
        mkdir package "$out"
        cp ${coopr-static}/bin/coopr package/coopr
        cp ${coopr-static}/share/licenses/coopr/LICENSE package/LICENSE
        cp -r ${coopr-static}/share/licenses/coopr/third-party package/third-party
        tar ${tarFlags} -C package -cf - coopr LICENSE third-party | gzip -n > "$out/coopr-linux-${arch}.tar.gz"
      '';
  sources =
    pkgs.runCommand "coopr-sources.tar.gz"
      {
        nativeBuildInputs = [
          pkgs.gnutar
          pkgs.gzip
        ];
      }
      ''
        mkdir "$out"
        tar ${tarFlags} -C ${release-sources} -cf - . | gzip -n > "$out/coopr-sources.tar.gz"
      '';
in
{
  inherit binary sources;
  check =
    pkgs.runCommand "coopr-release-check"
      {
        nativeBuildInputs = [
          pkgs.gnutar
          pkgs.gzip
          pkgs.binutils
        ];
      }
      ''
        mkdir package
        tar -xzf ${binary}/coopr-linux-${arch}.tar.gz -C package
        cmp package/coopr ${coopr-static}/bin/coopr
        cmp package/LICENSE ${../LICENSE}
        diff -r package/third-party ${coopr-static}/share/licenses/coopr/third-party
        test -s package/third-party/NOTICE
        test -s package/third-party/gpgme/COPYING.LESSER
        if readelf -l package/coopr | grep -q INTERP; then
          echo 'The release binary requires a dynamic loader' >&2
          exit 1
        fi
        env -i PATH=/nonexistent HOME="$TMPDIR" ./package/coopr --help > help
        grep -q 'Usage:' help
        test "$(env -i PATH=/nonexistent HOME="$TMPDIR" ./package/coopr --version)" = ${pkgs.lib.escapeShellArg "coopr version ${coopr-static.version}"}
        touch "$out"
      '';
  sourceCheck =
    pkgs.runCommand "coopr-source-release-check"
      {
        nativeBuildInputs = [
          pkgs.gnutar
          pkgs.gzip
        ];
      }
      ''
        mkdir sources
        tar -xzf ${sources}/coopr-sources.tar.gz -C sources
        diff -r sources ${release-sources}
        test -s sources/REBUILD.txt
        test -s sources/coopr/nix/package-static.nix
        touch "$out"
      '';
}
