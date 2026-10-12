{
  runCommand,
  zensical,
  chroma,
  python3,
}:
runCommand "coopr-docs"
  {
    nativeBuildInputs = [
      zensical
      chroma
      (python3.withPackages (ps: [ ps.pymdown-extensions ]))
    ];
  }
  ''
    mkdir source
    cp -r ${../docs} source/docs
    cp ${../zensical.toml} source/zensical.toml
    chmod -R u+w source
    cd source
    export HOME="$TMPDIR"
    python3 -B -m unittest discover -s docs/.overrides -p 'test_*.py'
    zensical build --clean --strict
    test ! -e site/.overrides
    cmp docs/install.sh site/install.sh
    mv site "$out"
  ''
