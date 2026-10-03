{
  lib,
  pkgs,
  coopr,
  zensical,
}:
pkgs.mkShell {
  inputsFrom = [ coopr ];
  packages = with pkgs; [
    coopr
    go_1_27
    gopls
    gotools
    golangci-lint
    govulncheck
    (runCommand "coopr-test-busybox" { } ''
      mkdir -p "$out/bin"
      cp ${pkgsStatic.busybox}/bin/busybox "$out/bin/busybox"
    '')
    delve
    git
    openssh
    gnupg
    jq
    just
    nixfmt
    crun
    e2fsprogs
    aardvark-dns
    netavark
    passt
    slirp4netns
    podman
    shellcheck
    zensical
    cacert
  ];
  inherit (coopr) CGO_ENABLED;
  GOFLAGS = "-tags=${lib.concatStringsSep "," coopr.tags}";
}
