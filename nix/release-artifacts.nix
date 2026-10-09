{
  pkgs,
  coopr-static,
  release-sources,
  nix2container,
  sourceUrl,
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
          pkgs.pigz
        ];
      }
      ''
        mkdir "$out"
        # Nix outputs are read-only; extracted corresponding source must be editable.
        tar ${tarFlags} --mode=u+w -C ${release-sources} -cf - . | pigz -p"$NIX_BUILD_CORES" -nTR > "$out/coopr-sources.tar.gz"
      '';
  sourceImage = nix2container.buildImage {
    name = "coopr";
    tag = "source-${pkgs.lib.replaceStrings [ "+" ] [ "_" ] coopr-static.version}";
    copyToRoot = sources;
    config.Labels = {
      "org.opencontainers.image.title" = "Coopr corresponding source";
      "org.opencontainers.image.version" = coopr-static.version;
      "org.opencontainers.image.source" = sourceUrl;
    };
  };
in
{
  inherit binary sources sourceImage;
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
        test -s package/third-party/go/LICENSE
        test -s package/third-party/go-modules/go.podman.io/buildah/LICENSE
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
          pkgs.regclient.regctl
          pkgs.jq
          sourceImage.copyTo
        ];
      }
      ''
        test -s ${sources}/coopr-sources.tar.gz
        test "$(stat -c %s ${sources}/coopr-sources.tar.gz)" -lt 2147483648
        copy-to --tmpdir "$TMPDIR" --format oci oci:layout:source
        test "$(jq -r .imageLayoutVersion layout/oci-layout)" = 1.0.0
        regctl image inspect --format raw-body ocidir://layout:source > config.json
        jq -e --arg version ${pkgs.lib.escapeShellArg coopr-static.version} \
          --arg source ${pkgs.lib.escapeShellArg sourceUrl} '
          .config.Labels["org.opencontainers.image.version"] == $version and
          .config.Labels["org.opencontainers.image.source"] == $source and
          ((.config.Entrypoint // []) | length == 0) and
          ((.config.Cmd // []) | length == 0)
        ' config.json
        regctl image get-file ocidir://layout:source /coopr-sources.tar.gz image-sources.tar.gz
        cmp image-sources.tar.gz ${sources}/coopr-sources.tar.gz
        mkdir sources
        tar -xzf ${sources}/coopr-sources.tar.gz -C sources
        diff -r --no-dereference sources ${release-sources}
        test -s sources/REBUILD.txt
        test -s sources/coopr/nix/package-static.nix
        test -s sources/coopr/nix/container-licenses.nix
        test -w sources/coopr/nix/package-static.nix
        test -w sources/coopr
        mkdir nixpkgs
        tar -xzf sources/build-inputs/nixpkgs.tar.gz -C nixpkgs
        test -w nixpkgs/pkgs/by-name/cr/crun/package.nix
        test -w nixpkgs
        test -d sources/container
        test -d sources/sources
        test -s sources/vendor/modules.txt
        test ! -e sources/go
        test -z "$(find sources/container -maxdepth 1 \( -name 'coopr-*' -o -name 'aardvark-dns-*' -o -name 'netavark-*' -o -name 'libkrun*' \) -print -quit)"
        # Source links must remain usable after extracting the archive away from Nix.
        for component in sources/libraries/* sources/container/*; do
          for source in "$component"/*; do
            if [ -L "$source" ]; then
              test -e "$source"
              case "$(readlink "$source")" in
                ../../sources/*) ;;
                *) echo "Unexpected source link: $source" >&2; exit 1 ;;
              esac
            fi
          done
        done
        # GCC runtime/static variants share the same upstream source archive.
        test "$(find sources/sources -maxdepth 1 -name '*gcc-*.tar.*' | wc -l)" = 1
        test "$(find sources/libraries/gpgme -maxdepth 1 -name '*.tar.bz2' -type l | wc -l)" = 1
        test -n "$(find sources/container/gnupg-${pkgs.gnupg.version}/patches -name '*0002-gpg-accept-subkeys*' -type f -size +0c -print -quit)"
        test -n "$(find sources/container/git-minimal-${pkgs.gitMinimal.version}/patches -name '*t7703-ignore-ls-total.patch' -type f -size +0c -print -quit)"
        touch "$out"
      '';
}
