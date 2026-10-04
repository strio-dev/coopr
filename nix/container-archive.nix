{ runCommand, container }:
runCommand "coopr-container.tar" { nativeBuildInputs = [ container.copyTo ]; } ''
  copy-to --tmpdir "$TMPDIR" "docker-archive:$out:localhost/coopr:nix"
''
