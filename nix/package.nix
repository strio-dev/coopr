{
  lib,
  buildGoModule,
  buildPackages,
  go_1_27,
  pkg-config,
  gpgme,
  libseccomp,
  version ? "dev",
}:
(buildGoModule.override { go = go_1_27; }) (finalAttrs: {
  pname = "coopr";
  inherit version;
  src = lib.fileset.toSource {
    root = ../.;
    fileset = lib.fileset.unions [
      ../cmd
      ../internal
      ../go.mod
      ../go.sum
      ../LICENSE
      ../examples
    ];
  };
  vendorHash = "sha256-P6E5VjQ/41JN0bH/lfLJa8Hqc3OVfoP014GUhsamRLw=";
  subPackages = [ "./cmd/coopr" ];
  ldflags = [ "-X main.version=${version}" ];
  tags = [
    "exclude_graphdriver_btrfs"
    "systemd"
    "seccomp"
  ];
  # Compatible tests, vet, and lint share a flake check; native ARM tests remain separate.
  doCheck = false;
  env.CGO_ENABLED = "1";
  nativeBuildInputs = [
    pkg-config
    buildPackages.gnutar
    buildPackages.gzip
  ];
  buildInputs = [
    gpgme
    libseccomp
  ];
  postInstall =
    let
      vendorSource =
        if finalAttrs.vendorHash == null then "${finalAttrs.src}/vendor" else finalAttrs.goModules;
    in
    ''
      install -Dm644 LICENSE "$out/share/licenses/coopr/LICENSE"
      mkdir -p "$out/share/licenses/coopr/third-party/go"
      for file in LICENSE PATENTS; do
        tar -xOf ${go_1_27.src} "go/$file" > "$out/share/licenses/coopr/third-party/go/$file"
      done
      if [ -f ${vendorSource}/modules.txt ]; then
        install -Dm644 ${vendorSource}/modules.txt "$out/share/licenses/coopr/third-party/go-modules/modules.txt"
      fi
      while IFS= read -r -d "" file; do
        relative="''${file#${vendorSource}/}"
        install -Dm644 "$file" "$out/share/licenses/coopr/third-party/go-modules/$relative"
      done < <(find ${vendorSource} -type f \( \
        -iname 'license*' -o -iname 'licence*' -o -iname 'copying*' \
        -o -iname 'notice*' -o -iname 'copyright*' -o -iname 'patents*' \
        -o -iname 'authors*' -o -iname 'unlicense*' \
        -o -ipath '*/licenses/*' \) ! -iname '*.go' ! -iname '*.proto' -print0)
    '';
  meta = {
    license = lib.licenses.mit;
    mainProgram = "coopr";
  };
})
