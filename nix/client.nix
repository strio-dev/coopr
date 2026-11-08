{
  lib,
  buildGoModule,
  go_1_27,
  coopr,
}:
(buildGoModule.override { go = go_1_27; }) {
  pname = "coopr-client-consumer";
  version = "dev";
  src = coopr.testSource;
  modRoot = "examples/client";
  # The only module dependency is the local source tree selected by replace.
  # Build it offline instead of copying its own packages into vendor.
  proxyVendor = true;
  vendorHash = null;
  subPackages = [ "." ];
  env.CGO_ENABLED = "0";
  doCheck = true;
  postCheck = ''
    go vet ./...
  '';
  postInstall = ''
    mkdir -p "$out/share/coopr-client-consumer"
    go list -deps -f '{{.ImportPath}} {{.Standard}} {{len .CgoFiles}}' . \
      > "$out/share/coopr-client-consumer/dependencies.txt"
    go version -m "$out/bin/client-consumer" > "$out/share/coopr-client-consumer/build-info.txt"
    grep -F 'CGO_ENABLED=0' "$out/share/coopr-client-consumer/build-info.txt"
  '';
  meta.license = lib.licenses.mit;
}
