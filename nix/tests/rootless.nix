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
  integrationTests = coopr.overrideAttrs {
    pname = "coopr-rootless-tests";
    goModules = coopr.goModules;
    doCheck = false;
    buildPhase = ''
      runHook preBuild
      mkdir -p "$out/bin"
      for package in cmd/coopr internal/build internal/buildah internal/transfer; do
        go test -trimpath -c -o "$out/bin/$(basename "$package").test" "./$package"
      done
      runHook postBuild
    '';
    installPhase = "runHook preInstall; runHook postInstall";
    postInstall = "";
    env = {
      inherit (coopr) CGO_ENABLED;
      GOFLAGS = "-tags=${lib.concatStringsSep "," coopr.tags}";
    };
  };
  runTests = pkgs.writeShellScript "coopr-rootless-tests" ''
    set -euo pipefail
    test "$(id -u)" = 1000
    export XDG_RUNTIME_DIR=/run/user/1000
    export DBUS_SESSION_BUS_ADDRESS="unix:path=$XDG_RUNTIME_DIR/bus"
    export PATH="${pkgs.git}/libexec/git-core:$PATH"
    export TMPDIR=/tmp
    export GOPROXY=off
    export GOSUMDB=off
    export GOCACHE="$HOME/.cache/go-build"
    export CGO_ENABLED=0
    export GOFLAGS="-mod=vendor -tags=${lib.concatStringsSep "," coopr.tags}"
    export COOPR_TEST_BUILDAH=1
    export COOPR_TEST_BUILDAH_REGISTRY=1
    export COOPR_TEST_CONTAINER_STORAGE=1
    export COOPR_TEST_CLI=${coopr}/bin/coopr
    export COOPR_TEST_BINFMT_HANDLER=${foreignSystem}
    export COOPR_ACCEPTANCE_IMAGE_COPY=${container.copyTo}/bin/copy-to
    systemctl --user show-environment >/dev/null
    test "$(podman info --format '{{.Host.Security.Rootless}}')" = true
    mkdir -p "$GOCACHE" "$HOME/work"
    cp -r ${coopr.src}/. "$HOME/work/"
    cp -r ${acceptanceSource}/. "$HOME/work/"
    cp -r ${coopr.goModules} "$HOME/work/vendor"
    chmod -R u+w "$HOME/work"
    cd "$HOME/work"
    for package in cmd/coopr internal/build internal/buildah internal/transfer; do
      (cd "$package" && ${integrationTests}/bin/"$(basename "$package")".test -test.v -test.count=1 -test.timeout=30m) | tee "$HOME/$(basename "$package").test.log"
    done
    grep -q '^--- PASS: TestBuildPlanExecutesForeignBinaryThroughRegisteredBinfmt ' "$HOME/buildah.test.log"
    bash scripts/acceptance/release.sh
    COOPR_ACCEPTANCE_CLI=${coopr-static}/bin/coopr bash scripts/acceptance/release.sh
  '';
in
pkgs.testers.runNixOSTest {
  name = "coopr-rootless";
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
    machine.succeed("su - coopr -c '${runTests}'", timeout=85 * 60)
  '';
}
