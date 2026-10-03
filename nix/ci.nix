{ inputs, self, ... }:
{
  flake.githubActions = inputs.nix-github-actions.lib.mkGithubMatrix {
    inherit (self) checks;
  };
}
