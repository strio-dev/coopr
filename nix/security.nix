{
  fetchFromGitHub,
  fetchurl,
  jq,
  mkShellNoCC,
  patchelf,
  python3Packages,
  runCommand,
  semgrep,
}:
let
  # patchelf 0.19.3 fixes Semgrep's ELF LOAD overlap (NixOS/patchelf#672).
  # Scope the newer tool to this package, retaining its single rewrite.
  semgrepPatchelf = patchelf.overrideAttrs (
    finalAttrs: _previousAttrs: {
      version = "0.19.3";
      src = fetchurl {
        url = "https://github.com/NixOS/patchelf/releases/download/${finalAttrs.version}/patchelf-${finalAttrs.version}.tar.bz2";
        hash = "sha256-INviPp28StdpB4qGetACwEKXoGn/k8lftyJC3VXU3W8=";
      };
    }
  );
  securitySemgrep = semgrep.override {
    semgrep-core = semgrep.semgrep-core.override { patchelf = semgrepPatchelf; };
    # Semgrep 1.180.0 declares mcp==1.29.0; MCP 2 renamed FastMCP.
    mcp = python3Packages.mcp_1;
  };
  source = fetchFromGitHub {
    owner = "semgrep";
    repo = "semgrep-rules";
    rev = "16b9e6d45f10649f6a531ec4838ed9924372c3bc";
    hash = "sha256-YDvqlGgcDLGiJF2W2pAgT+9gF2GLfuyDSDa9E5q+qVk=";
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
  packages = [
    securitySemgrep
    jq
  ];
  # Relative config paths keep rule namespaces stable across Nix store hashes.
  SEMGREP_RULES = ".cache/semgrep-rules";
  SEMGREP_SEND_METRICS = "off";
  SEMGREP_ENABLE_VERSION_CHECK = "0";
  shellHook = ''
    mkdir -p .cache && ln -sfnT ${rules} .cache/semgrep-rules || exit 1
  '';
}
