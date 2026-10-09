{
  lib,
  pkgs,
  coopr,
  coopr-static,
  container,
}:
let
  foreignSystem = "aarch64-linux";
  runtime = pkgs.callPackage ../devshell.nix {
    inherit coopr;
    withDevelopmentTools = false;
  };
  acceptanceSource = lib.fileset.toSource {
    root = ../..;
    fileset = ../../scripts/acceptance;
  };
  fixtures = pkgs.callPackage ./fixtures.nix { inherit coopr; };
  buildahShards = 8;
  integrationTests = coopr.overrideAttrs {
    pname = "coopr-rootless-tests";
    src = coopr.testSource;
    goModules = coopr.goModules;
    doCheck = false;
    buildPhase = ''
      runHook preBuild
      mkdir -p "$out/bin"
      for package in cmd/coopr internal/build internal/buildah internal/transfer; do
        go test -trimpath -c -o "$out/bin/$(basename "$package").test" "./$package"
      done
      go build -trimpath -o "$out/bin/test2json" cmd/test2json
      mkdir -p "$out/shards"
      "$out/bin/buildah.test" -test.list '^(Test|Example|Fuzz)' > "$out/buildah.inventory"
      test -s "$out/buildah.inventory"
      split -d -n r/${toString buildahShards} --additional-suffix=.txt \
        "$out/buildah.inventory" "$out/shards/buildah-"
      for shard in "$out"/shards/*.txt; do
        test -s "$shard"
      done
      sort "$out/buildah.inventory" > expected
      sort -u "$out/buildah.inventory" > unique
      diff -u expected unique
      sort "$out"/shards/*.txt > actual
      diff -u expected actual
      runHook postBuild
    '';
    installPhase = "runHook preInstall; runHook postInstall";
    postInstall = "";
    env = {
      inherit (coopr) CGO_ENABLED;
      GOFLAGS = "-tags=${lib.concatStringsSep "," coopr.tags}";
    };
  };
  makeTest =
    name:
    {
      packages ? [ ],
      shard ? null,
      acceptance ? null,
    }:
    let
      testPackages = if shard != null then [ "internal/buildah" ] else packages;
      runTests = pkgs.writeShellApplication {
        inherit name;
        runtimeEnv = {
          XDG_RUNTIME_DIR = "/run/user/1000";
          DBUS_SESSION_BUS_ADDRESS = "unix:path=/run/user/1000/bus";
          TMPDIR = "/tmp";
          GOPROXY = "off";
          GOSUMDB = "off";
          CGO_ENABLED = "0";
          GOFLAGS = "-mod=vendor -tags=${lib.concatStringsSep "," coopr.tags}";
          COOPR_TEST_BUILDAH = "1";
          COOPR_TEST_BUILDAH_REGISTRY = "1";
          COOPR_TEST_CONTAINER_STORAGE = "1";
          COOPR_TEST_CLI = "${coopr}/bin/coopr";
          COOPR_TEST_BINFMT_HANDLER = foreignSystem;
          COOPR_TEST_FIXTURES = fixtures;
        }
        // lib.optionalAttrs (acceptance != null) {
          COOPR_ACCEPTANCE_IMAGE_COPY = "${container.copyTo}/bin/copy-to";
          COOPR_ACCEPTANCE_TEST_BINARY = "${integrationTests}/bin/build.test";
        }
        // lib.optionalAttrs (acceptance == "static") {
          COOPR_ACCEPTANCE_CLI = "${coopr-static}/bin/coopr";
        };
        text = ''
          test "$(id -u)" = 1000
          export PATH="${pkgs.git}/libexec/git-core:$PATH"
          export GOCACHE="$HOME/.cache/go-build"
          systemctl --user show-environment >/dev/null
          test "$(podman info --format '{{.Host.Security.Rootless}}')" = true
          mkdir -p "$GOCACHE" "$HOME/work"
          cp -r ${coopr.testSource}/. "$HOME/work/"
          cp -r ${acceptanceSource}/. "$HOME/work/"
          cp -r ${coopr.goModules} "$HOME/work/vendor"
          chmod -R u+w "$HOME/work"
          cd "$HOME/work"
          ${lib.optionalString (shard != null) ''
            selected=${integrationTests}/shards/buildah-${lib.fixedWidthNumber 2 shard}.txt
            printf 'Running Buildah shard %s/${toString buildahShards}: %s tests\n' \
              '${toString (shard + 1)}' "$(wc -l < "$selected")"
            selector="^($(paste -sd '|' "$selected"))$"
          ''}
          ${lib.optionalString (testPackages != [ ]) ''
            test_packages=(${lib.escapeShellArgs testPackages})
            for package in "''${test_packages[@]}"; do
              printf 'Running integration tests: %s\n' "$package"
              time (cd "$package" && ${pkgs.gotestsum}/bin/gotestsum \
                --jsonfile "$HOME/$(basename "$package").test.json" --raw-command -- \
                ${integrationTests}/bin/test2json -t -p "./$package" \
                ${integrationTests}/bin/"$(basename "$package")".test \
                -test.v=test2json -test.count=1 -test.timeout=30m ${
                  lib.optionalString (shard != null) ''-test.run "$selector"''
                })
            done
          ''}
          ${lib.optionalString (shard != null) ''
            ${pkgs.jq}/bin/jq -r '
              select(.Action == "pass" or .Action == "skip" or .Action == "fail")
              | select(.Test != null and (.Test | contains("/") | not))
              | .Test
            ' "$HOME/buildah.test.json" | sort > completed
            sort "$selected" > expected
            diff -u expected completed
            if grep -qx TestBuildPlanExecutesForeignBinaryThroughRegisteredBinfmt "$selected"; then
              ${pkgs.jq}/bin/jq -es '
                any(.[]; .Test == "TestBuildPlanExecutesForeignBinaryThroughRegisteredBinfmt" and .Action == "pass")
              ' "$HOME/buildah.test.json" >/dev/null
            fi
          ''}
          ${lib.optionalString (acceptance != null) ''
            printf 'Running packaged acceptance: ${acceptance} CLI\n'
            time bash scripts/acceptance/release.sh
          ''}
        '';
      };
      runner = "${runTests}/bin/${name}";
    in
    pkgs.testers.runNixOSTest {
      inherit name;
      requiredFeatures.kvm = true;
      qemu.forceAccel = true;
      globalTimeout = 90 * 60;

      nodes.machine = {
        virtualisation = {
          memorySize = 4096;
          diskSize = 16384;
          cores = 4;
          podman.enable = true;
          containers.containersConf.settings.engine.helper_binaries_dir = [
            "${pkgs.netavark}/bin"
            "${pkgs.aardvark-dns}/bin"
          ];
          additionalPaths = [ runTests ];
        };
        boot.binfmt = {
          emulatedSystems = [ foreignSystem ];
          preferStaticEmulators = true;
        };
        systemd.services."user@".serviceConfig.Delegate = "pids memory cpu cpuset";
        users.users.coopr = {
          isNormalUser = true;
          uid = 1000;
          autoSubUidGidRange = true;
          linger = true;
        };
        environment.systemPackages = map (package: package.bin or package) runtime.nativeBuildInputs ++ [
          pkgs.diffutils
          pkgs.gnutar
        ];
      };

      testScript = ''
        machine.wait_for_unit("default.target")
        machine.wait_for_unit("user@1000.service")
        machine.succeed("grep -qx enabled /proc/sys/fs/binfmt_misc/${foreignSystem}")
        machine.succeed("grep -q '^flags:.*F' /proc/sys/fs/binfmt_misc/${foreignSystem}")
        machine.succeed("su - coopr -c '${runner}' 2>&1 | tee /dev/ttyS0", timeout=85 * 60)
      '';
    };
in
{
  rootless-native = makeTest "coopr-rootless-native" {
    packages = [
      "cmd/coopr"
      "internal/build"
      "internal/transfer"
    ];
  };
  rootless-packaged = makeTest "coopr-rootless-packaged" { acceptance = "dynamic"; };
  rootless-static = makeTest "coopr-rootless-static" { acceptance = "static"; };
}
// lib.listToAttrs (
  lib.genList (shard: {
    name = "rootless-buildah-${toString (shard + 1)}";
    value = makeTest "coopr-rootless-buildah-${toString (shard + 1)}" { inherit shard; };
  }) buildahShards
)
