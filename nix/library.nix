{
  lib,
  stdenv,
  buildGoModule,
  go_1_27,
  golangci-lint,
  coopr,
}:
(buildGoModule.override { go = go_1_27; }) {
  pname = "coopr-library-consumer";
  version = "dev";
  src = coopr.testSource;
  modRoot = "examples/library";
  vendorHash = "sha256-eUnHeKTx4JJs3F0UWhbsm/dK0OkNuwWwPc1mBrTXGy4=";
  subPackages = [ "." ];
  inherit (coopr) tags nativeBuildInputs buildInputs;
  env.CGO_ENABLED = "1";
  doCheck = true;
  nativeCheckInputs = lib.optionals stdenv.hostPlatform.isx86_64 [ golangci-lint ];
  postCheck = lib.optionalString stdenv.hostPlatform.isx86_64 ''
    go vet -tags=${lib.concatStringsSep "," coopr.tags} ./...
    GOLANGCI_LINT_CACHE="$TMPDIR/golangci-lint" golangci-lint run --build-tags=${lib.concatStringsSep "," coopr.tags}
  '';
  meta.license = lib.licenses.mit;
}
