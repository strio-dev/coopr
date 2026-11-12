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
  # The local Coopr replacement is vendored, so source edits also change this hash.
  vendorHash = "sha256-NupaHt/h/FCzl8OayuPbCsI5ekhXaZHMkaweyz8V6uA=";
  subPackages = [ "." ];
  inherit (coopr) tags nativeBuildInputs buildInputs;
  env.CGO_ENABLED = "1";
  doCheck = true;
  nativeCheckInputs = lib.optionals stdenv.hostPlatform.isx86_64 [ golangci-lint ];
  postCheck = lib.optionalString stdenv.hostPlatform.isx86_64 ''
    export GOMAXPROCS="$NIX_BUILD_CORES" GOMEMLIMIT=2GiB
    export GOFLAGS="$GOFLAGS -p=$NIX_BUILD_CORES"
    go vet -tags=${lib.concatStringsSep "," coopr.tags} ./...
    GOLANGCI_LINT_CACHE="$TMPDIR/golangci-lint" golangci-lint run --build-tags=${lib.concatStringsSep "," coopr.tags}
  '';
  meta.license = lib.licenses.mit;
}
