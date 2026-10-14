{
  lib,
  pkgs,
  coopr,
  podman-sequoia,
  rustPlatform,
}:
let
  nativeRoots = coopr.buildInputs ++ [
    podman-sequoia
    pkgs.stdenv.cc.libc
    pkgs.stdenv.cc.cc.lib
    pkgs.glibc.libgcc
    pkgs.libidn2.out
    pkgs.gnupg
    pkgs.pcsclite.lib
    pkgs.systemdLibs
    pkgs.libtool.lib
    pkgs.mailcap
    pkgs.dns-root-data
    pkgs.iana-etc
    pkgs.tzdata
  ];
  nativeAttribution = import ./container-licenses.nix {
    inherit pkgs coopr;
    roots = nativeRoots;
    runtimeRoots = nativeRoots;
  };
  sources = pkgs.runCommand "coopr-sequoia-signing-${coopr.version}-sources" { } ''
    mkdir -p "$out/coopr" "$out/build-inputs"
    cp -r ${coopr.testSource}/. "$out/coopr/"
    cp -r ${./.} "$out/coopr/nix"
    cp ${../flake.nix} "$out/coopr/flake.nix"
    cp ${../flake.lock} "$out/coopr/flake.lock"
    cp -r ${coopr.goModules} "$out/vendor"
    cp -r ${nativeAttribution.licenseSources}/. "$out/"
    cp ${./podman-sequoia.nix} "$out/build-inputs/podman-sequoia.nix"
    cp ${./sequoia-helper.nix} "$out/build-inputs/sequoia-helper.nix"
    cp ${rustPlatform.rust.rustc.src} "$out/build-inputs/rustc-source.tar.gz"
    cat > "$out/REBUILD.txt" <<'REBUILD'
    Optional Coopr Sequoia signing helper sources

    Build with the pinned flake inputs from the coopr directory:
      nix build path:.#coopr-sequoia-signing

    The coopr directory contains Coopr's source and Nix build expressions.
    vendor contains the pinned Go dependencies. Native upstream sources,
    Cargo dependencies, and stock Nixpkgs patches are indexed under container
    and sources; the Rust toolchain source is under build-inputs.

    Native signing uses unmodified podman-sequoia ${podman-sequoia.version}, its Cargo.lock, and
    the default OpenSSL backend. The linker embeds that library's Nix store
    directory. The optional binary requires its Nix runtime closure.
    REBUILD
  '';
in
coopr.overrideAttrs (old: {
  pname = "coopr-sequoia-signing";
  subPackages = [ "./cmd/coopr-sequoia-signing" ];
  tags = old.tags ++ [ "containers_image_sequoia" ];
  ldflags = old.ldflags ++ [
    "-X go.podman.io/image/v5/signature/internal/sequoia.sequoiaLibraryDir=${podman-sequoia}/lib"
  ];
  postInstall =
    # Preserve the shared Go/license installation without building Coopr's
    # unrelated runtime helper in this separately installed signing package.
    lib.replaceStrings
      [
        ''CGO_ENABLED=0 go build -trimpath -o "$out/libexec/coopr/coopr-run-sequence" ./cmd/coopr-run-sequence''
      ]
      [ "" ]
      old.postInstall
    + ''
      cp -r ${nativeAttribution.licenseBundle}/share/licenses/coopr/container "$out/share/licenses/coopr/native"
      chmod u+w "$out/share/licenses/coopr/native"
      mkdir -p "$out/share/licenses/coopr/native/rust"
      tar -xzf ${rustPlatform.rust.rustc.src} \
        --strip-components=1 -C "$out/share/licenses/coopr/native/rust" \
        rustc-${rustPlatform.rust.rustc.version}-src/LICENSE-APACHE \
        rustc-${rustPlatform.rust.rustc.version}-src/LICENSE-MIT \
        rustc-${rustPlatform.rust.rustc.version}-src/COPYRIGHT \
        rustc-${rustPlatform.rust.rustc.version}-src/LICENSES

    '';
  passthru = (old.passthru or { }) // {
    inherit podman-sequoia nativeAttribution sources;
  };
  meta = old.meta // {
    description = "Optional native Sequoia signing transfer helper for Coopr";
    mainProgram = "coopr-sequoia-signing";
    platforms = lib.platforms.linux;
  };
})
