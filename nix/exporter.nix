{
  lib,
  buildGoModule,
  go_1_27,
}:
(buildGoModule.override { go = go_1_27; }) (finalAttrs: {
  pname = "coopr-nix-export";
  version = "dev";
  src = lib.fileset.toSource {
    root = ./exporter;
    fileset = lib.fileset.unions [
      ./exporter/go.mod
      ./exporter/go.sum
      (lib.fileset.fileFilter (file: file.hasExt "go") ./exporter)
    ];
  };
  vendorHash = "sha256-XrZhmd+F04swWpQaPRXbF+mBbMIyXx17S/HnruodnA8=";
  env.CGO_ENABLED = "0";
  preBuild = ''
    export GOMAXPROCS="$NIX_BUILD_CORES"
    export GOMEMLIMIT=1536MiB
    export GOFLAGS="$GOFLAGS -p=$NIX_BUILD_CORES"
  '';
  postCheck = ''
    unformatted="$(gofmt -l ./*.go)"
    if [ -n "$unformatted" ]; then
      printf 'Exporter Go files need gofmt:\n%s\n' "$unformatted" >&2
      exit 1
    fi
    go vet ./...
  '';
  ldflags = [
    "-s"
    "-w"
  ];
  postInstall = ''
    mv "$out/bin/exporter" "$out/bin/coopr-nix-export"
    install -Dm644 ${../LICENSE} "$out/share/licenses/coopr-nix-export/LICENSE"
    mkdir -p "$out/share/licenses/coopr-nix-export/go"
    for file in LICENSE PATENTS; do
      tar -xOf ${go_1_27.src} "go/$file" > "$out/share/licenses/coopr-nix-export/go/$file"
    done
    vendor_source=${finalAttrs.goModules}
    install -Dm644 "$vendor_source/modules.txt" "$out/share/licenses/coopr-nix-export/go-modules/modules.txt"
    while IFS= read -r -d "" file; do
      relative="''${file#$vendor_source/}"
      install -Dm644 "$file" "$out/share/licenses/coopr-nix-export/go-modules/$relative"
    done < <(find "$vendor_source" -type f \( \
      -iname 'license*' -o -iname 'licence*' -o -iname 'copying*' \
      -o -iname 'notice*' -o -iname 'copyright*' -o -iname 'patents*' \
      -o -iname 'authors*' -o -iname 'unlicense*' -o -ipath '*/licenses/*' \) \
      ! -iname '*.go' ! -iname '*.proto' -print0)
  '';
  meta = {
    mainProgram = "coopr-nix-export";
    license = lib.licenses.mit;
  };
})
