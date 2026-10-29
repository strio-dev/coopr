{
  lib,
  buildGoModule,
  go_1_27,
  coopr,
}:
(buildGoModule.override { go = go_1_27; }) {
  pname = "coopr-library-consumer";
  version = "dev";
  src = coopr.testSource;
  modRoot = "examples/library";
  vendorHash = "sha256-s9FqQvaz/oLAiSdx7iJGvadriAKiuMkbjwmjzC4Ia+s=";
  subPackages = [ "." ];
  inherit (coopr) tags nativeBuildInputs buildInputs;
  env.CGO_ENABLED = "1";
  doCheck = true;
  meta.license = lib.licenses.mit;
}
