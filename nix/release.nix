{
  mkShellNoCC,
  coreutils,
  regclient,
}:
mkShellNoCC {
  packages = [
    coreutils
    regclient.regctl
  ];
}
