{
  lib,
  buildGoModule,
  go_1_27,
  pkg-config,
  gpgme,
  libseccomp,
}:
(buildGoModule.override { go = go_1_27; }) {
  pname = "coopr";
  version = "0.1.0-dev";
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
  vendorHash = "sha256-P/ipWaeaN9OXg9m7bnnxSik7yurkycxkssHMg73BNMU=";
  subPackages = [ "./cmd/coopr" ];
  tags = [
    "exclude_graphdriver_btrfs"
    "systemd"
    "seccomp"
  ];
  # Tests, vet, and lint are separate flake checks.
  doCheck = false;
  env.CGO_ENABLED = "1";
  nativeBuildInputs = [ pkg-config ];
  buildInputs = [
    gpgme
    libseccomp
  ];
  postInstall = ''
    install -Dm644 LICENSE "$out/share/licenses/coopr/LICENSE"
  '';
  meta = {
    license = lib.licenses.mit;
    mainProgram = "coopr";
  };
}
