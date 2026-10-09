{
  pkgs,
  coopr,
}:
coopr.overrideAttrs {
  pname = "coopr-runtime-fixtures";
  src = pkgs.lib.fileset.toSource {
    root = ../..;
    fileset = pkgs.lib.fileset.unions [
      ../../internal/build/testdata/storage-runner
      ../../internal/buildah/testdata/sshgitserver
      ../../internal/buildah/testdata/foreign-proof
      ../../scripts/acceptance/fixtures/platform-proof
      ../../go.mod
      ../../go.sum
    ];
  };
  goModules = coopr.goModules;
  doCheck = false;
  buildInputs = [ ];
  buildPhase = ''
    runHook preBuild
    mkdir -p "$out"
    go build -trimpath -o "$out/storage-runner" ./internal/build/testdata/storage-runner
    go build -trimpath -o "$out/sshgitserver" ./internal/buildah/testdata/sshgitserver
    for arch in amd64 arm64; do
      GOOS=linux GOARCH="$arch" go build -trimpath \
        -o "$out/foreign-proof-$arch" ./internal/buildah/testdata/foreign-proof
      GOOS=linux GOARCH="$arch" go build -trimpath \
        -o "$out/acceptance-proof-$arch" ./scripts/acceptance/fixtures/platform-proof/main.go
    done
    runHook postBuild
  '';
  installPhase = "runHook preInstall; runHook postInstall";
  postInstall = "";
  env = {
    CGO_ENABLED = "0";
    GOOS = "linux";
    GOARCH = pkgs.stdenv.hostPlatform.go.GOARCH;
    GOFLAGS = "-mod=vendor";
  };
}
