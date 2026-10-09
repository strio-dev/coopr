{
  fetchFromGitHub,
  mkShellNoCC,
  runCommand,
  semgrep,
}:
let
  source = fetchFromGitHub {
    owner = "semgrep";
    repo = "semgrep-rules";
    rev = "9bbf021d0acffa3095fcffd17b2bc7787eb4796d";
    hash = "sha256-3PbjZ07ABF6aBCGmARihUqkL5EsvF4dkkQFdCjpyNfY=";
  };
  rules = runCommand "coopr-semgrep-rules" { } ''
    mkdir -p "$out/python/lang" "$out/yaml/github-actions"
    cd ${source}
    find go -type d -name security -prune \
      -exec cp -r --no-preserve=mode --parents -t "$out" {} +
    cp -r python/lang/security "$out/python/lang"
    cp -r yaml/github-actions/security "$out/yaml/github-actions"
    cp LICENSE "$out/LICENSE"
  '';
in
mkShellNoCC {
  packages = [ semgrep ];
  # Relative config paths keep rule namespaces stable across Nix store hashes.
  SEMGREP_RULES = ".cache/semgrep-rules";
  SEMGREP_SEND_METRICS = "off";
  SEMGREP_ENABLE_VERSION_CHECK = "0";
  shellHook = ''
    mkdir -p .cache && ln -sfnT ${rules} .cache/semgrep-rules || exit 1
  '';
}
