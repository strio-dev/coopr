{
  lib,
  rustPlatform,
  fetchFromGitHub,
  pkg-config,
  openssl_3_6,
  sqlite,
  stdenv,
  capnproto,
}:
rustPlatform.buildRustPackage (finalAttrs: {
  pname = "podman-sequoia";
  version = "0.4.0";
  src = fetchFromGitHub {
    owner = "ueno";
    repo = "podman-sequoia";
    tag = "v${finalAttrs.version}";
    hash = "sha256-sy/vfq6XWW6AjrFvHj8CyFbxa+yKWGwOLO3WUaI2Gsc=";
  };
  cargoHash = "sha256-B44cSEObcQhYw6NBiyacgCTkefo/lJFBTdpB2cNqAZg=";
  nativeBuildInputs = [
    pkg-config
    rustPlatform.bindgenHook
    capnproto
  ];
  buildInputs = [
    openssl_3_6
    sqlite
  ];
  env.OPENSSL_NO_VENDOR = "1";
  # Upstream supplies the ABI-versioned symlink alongside the Rust cdylib.
  installPhase = ''
    runHook preInstall
    install -Dm755 target/${stdenv.hostPlatform.rust.rustcTarget}/release/libpodman_sequoia.so "$out/lib/libpodman_sequoia.so"
    ln -s libpodman_sequoia.so "$out/lib/libpodman_sequoia.so.0"
    install -Dm644 LICENSE "$out/share/licenses/podman-sequoia/LICENSE"
    runHook postInstall
  '';
  meta = {
    description = "Sequoia OpenPGP signing backend for containers/image";
    homepage = "https://github.com/ueno/podman-sequoia";
    license = lib.licenses.asl20;
    platforms = lib.platforms.linux;
  };
})
