{
  inputs,
  self,
  lib,
  ...
}:
let
  categories = {
    quality.x86_64-linux = lib.getAttrs [
      "go-check"
      "formatting"
      "scripts"
      "justfile"
      "workflow"
      "release-source"
    ] self.checks.x86_64-linux;
    docs.x86_64-linux = lib.getAttrs [
      "docs"
      "docs-examples"
      "installer"
    ] self.checks.x86_64-linux;
    native = lib.genAttrs [ "x86_64-linux" "aarch64-linux" ] (
      system:
      lib.getAttrs (
        [
          "build"
          "container"
          "release-binary"
        ]
        ++ lib.optional (system == "x86_64-linux") "rootless"
        ++ lib.optional (system == "aarch64-linux") "test"
      ) self.checks.${system}
    );
  };
  matrices = lib.mapAttrs (
    name: checks:
    let
      generated = inputs.nix-github-actions.lib.mkGithubMatrix {
        inherit checks;
        attrPrefix = "checks";
      };
    in
    {
      include = map (
        system:
        let
          rows = builtins.filter (row: row.system == system) generated.matrix.include;
        in
        {
          inherit name system;
          os = (builtins.head rows).os;
          attrs = map (row: row.attr) rows;
        }
      ) (builtins.attrNames checks);
    }
  ) categories;
in
{
  flake.githubActions = {
    inherit matrices;
    checks = lib.foldl' lib.recursiveUpdate { } (builtins.attrValues categories);
    matrix.include = lib.concatMap (matrix: matrix.include) (builtins.attrValues matrices);
  };
}
