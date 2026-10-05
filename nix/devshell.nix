{
  lib,
  pkgs,
  coopr,
  zensical ? null,
  withDevelopmentTools ? true,
}:
pkgs.mkShell {
  inputsFrom = [ coopr ];
  packages =
    with pkgs;
    [
      coopr
      go_1_27
      (runCommand "coopr-test-busybox" { } ''
        mkdir -p "$out/bin"
        cp ${pkgsStatic.busybox}/bin/busybox "$out/bin/busybox"
      '')
      git
      openssh
      gnupg
      jq
      just
      crun
      e2fsprogs
      aardvark-dns
      netavark
      passt
      slirp4netns
      podman
      cacert
    ]
    ++ lib.optionals withDevelopmentTools [
      gopls
      gotools
      golangci-lint
      govulncheck
      delve
      nixfmt
      shellcheck
      zensical
    ];
  inherit (coopr) CGO_ENABLED;
  GOFLAGS = "-tags=${lib.concatStringsSep "," coopr.tags}";
}
