{ runCommand, zensical }:
runCommand "coopr-docs" { nativeBuildInputs = [ zensical ]; } ''
  mkdir source
  cp -r ${../docs} source/docs
  cp ${../zensical.toml} source/zensical.toml
  chmod -R u+w source
  cd source
  export HOME="$TMPDIR"
  zensical build --clean --strict
  cmp docs/install.sh site/install.sh
  mv site "$out"
''
