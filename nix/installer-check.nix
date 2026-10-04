{
  runCommand,
  python3,
  curl,
  gnutar,
  gzip,
  gawk,
}:
runCommand "coopr-installer-check"
  {
    nativeBuildInputs = [
      python3
      curl
      gnutar
      gzip
      gawk
    ];
  }
  ''
    python ${./tests/installer.py} ${../docs/install.sh}
    touch "$out"
  ''
