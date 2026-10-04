{
  lib,
  buildGoModule,
  go_1_27,
  pkg-config,
  gpgme,
  libseccomp,
  version ? "dev",
}:
(buildGoModule.override { go = go_1_27; }) {
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
  vendorHash = "sha256-VoZsv17TDOCMiClseSpzGiL6x5o2rf+7a4THscKbpw4=";
  subPackages = [ "./cmd/coopr" ];
  ldflags = [ "-X main.version=${version}" ];
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
