{
  lib,
  pkgs,
  coopr,
  coopr-sequoia-signing,
  coopr-static,
  container,
  libraryConsumer,
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
  # Ordered ownership keeps component/cache/source interactions in one group.
  # Core receives every test not matched here, including newly added tests.
  buildahGroups = [
    {
      name = "packages";
      pattern = "Package|Publish|Publication";
    }
    {
      name = "components";
      pattern = "Component";
    }
    {
      name = "cache";
      pattern = "Cache";
    }
    {
      name = "sources";
      pattern = "Copy|Copies|Add|Context|Source|Git|Transport";
    }
    {
      name = "runtime";
      pattern = "Run|Network|Mount|Security|Secret|SSH|Device|Binfmt";
    }
    {
      name = "workers";
      pattern = "Worker|Lifecycle|Cancel|Prune|Store|GraphExecutor|ReadyGraph";
    }
    {
      name = "image-metadata";
      pattern = "Image|Config|Metadata|History|Output|Manifest|Layer|Timestamp|Epoch|Checkpoint";
    }
  ];
  buildahGroupNames = map (group: group.name) buildahGroups ++ [ "core" ];
  integrationTests = (coopr.override { buildTestBinaries = true; }).overrideAttrs {
    pname = "coopr-rootless-tests";
    src = coopr.testSource;
    goModules = coopr.goModules;
    doCheck = false;
    subPackages = [
      "cmd/coopr"
      "internal/build"
      "internal/buildah"
      "internal/transfer"
      "internal/acceptance"
    ];
    postInstall = ''
      go build -trimpath -o "$out/bin/test2json" cmd/test2json
      mkdir -p "$out/groups"
      "$out/bin/buildah.test" -test.list '^(Test|Example|Fuzz)' > "$out/buildah.inventory"
      test -s "$out/buildah.inventory"
      cp "$out/buildah.inventory" remaining
      ${lib.concatMapStringsSep "\n" (group: ''
        grep -E ${lib.escapeShellArg group.pattern} remaining > "$out/groups/${group.name}.txt"
        grep -Ev ${lib.escapeShellArg group.pattern} remaining > next
        mv next remaining
      '') buildahGroups}
      mv remaining "$out/groups/core.txt"
      for group in "$out"/groups/*.txt; do
        test -s "$group"
      done
      sort "$out/buildah.inventory" > expected
      sort -u "$out/buildah.inventory" > unique
      diff -u expected unique
      sort "$out"/groups/*.txt > actual
      diff -u expected actual
    '';
    env = {
      inherit (coopr) CGO_ENABLED;
      GOFLAGS = "-trimpath -tags=${lib.concatStringsSep "," coopr.tags}";
    };
  };
  makeTest =
    name:
    {
      packages ? [ ],
      group ? null,
      acceptance ? null,
      withSequoia ? false,
    }:
    let
      testPackages = if group != null then [ "internal/buildah" ] else packages;
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
          COOPR_TEST_CLI = "${coopr}/bin/coopr";
          COOPR_TEST_BINFMT_HANDLER = foreignSystem;
          COOPR_TEST_FIXTURES = fixtures;
        }
        // lib.optionalAttrs (acceptance != null) {
          COOPR_ACCEPTANCE_IMAGE_COPY = "${container.copyTo}/bin/copy-to";
          COOPR_ACCEPTANCE_TEST_BINARY = "${integrationTests}/bin/acceptance.test";
        }
        // lib.optionalAttrs (acceptance == "static") {
          COOPR_ACCEPTANCE_CLI = "${coopr-static}/bin/coopr";
        };
        text = ''
          test "$(id -u)" = 1000
          export PATH="${pkgs.python3}/bin:${pkgs.git}/libexec/git-core:$PATH"
          ${lib.optionalString withSequoia ''
            export PATH="${
              lib.makeBinPath [
                coopr-sequoia-signing
                pkgs.sequoia-sq
              ]
            }:$PATH"
            command -v coopr-sequoia-signing
            command -v sq
            test "$(coopr-sequoia-signing --protocol-version)" = coopr-sequoia-signing/1
          ''}
          export GOCACHE="$HOME/.cache/go-build"
          systemctl --user show-environment >/dev/null
          test "$(podman info --format '{{.Host.Security.Rootless}}')" = true
          mkdir -p "$GOCACHE" "$HOME/work"
          cp -r ${coopr.testSource}/. "$HOME/work/"
          cp -r ${acceptanceSource}/. "$HOME/work/"
          cp -r ${coopr.goModules} "$HOME/work/vendor"
          chmod -R u+w "$HOME/work"
          cd "$HOME/work"
          ${lib.optionalString (group != null) ''
            selected=${integrationTests}/groups/${group}.txt
            printf 'Running Buildah group %s: %s tests\n' \
              '${group}' "$(wc -l < "$selected")"
            selector="^($(paste -sd '|' "$selected"))$"
          ''}
          ${lib.optionalString (lib.elem "internal/build" testPackages) ''
            ${libraryConsumer}/bin/library-consumer \
              -busybox "$(command -v busybox)" -work "$HOME/library-proof" \
              | tee "$HOME/library-proof.json"
            ${pkgs.jq}/bin/jq -e '
              .independent_branches_overlapped and .cleanup_verified and .writable_context_isolated
              and .warm.Hits > 0 and .cold.Stored > 0
              and .cancelled_processes_observed > 0
            ' "$HOME/library-proof.json" >/dev/null
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
                  lib.optionalString (group != null) ''-test.run "$selector"''
                })
            done
          ''}
          ${lib.optionalString (lib.elem "cmd/coopr" testPackages) ''
            for required in \
              TestNativeHistoryJSONFormatsEmpty \
              TestNativeInfoTemplateFunctions \
              TestNativeSaveConfiguredMultiImageAndExplicitOverride \
              TestNativePullImplicitDockerAuthAndExplicitMissingFile \
              TestNativeSaveLoadTerminalGuards \
              TestNativeComponentArchiveTerminalGuards \
              TestNativeBuildImplicitDockerAuth \
              TestBuildRemoteAddTLSOptions \
              TestBuildCommandsCacheTTLControls; do
              ${pkgs.jq}/bin/jq -es --arg required "$required" '
                any(.[]; .Test == $required and .Action == "pass")
              ' "$HOME/coopr.test.json" >/dev/null
            done
          ''}
          ${lib.optionalString (lib.elem "internal/build" testPackages) ''
            ${pkgs.jq}/bin/jq -es '
              any(.[]; .Test == "TestPinnedAlwaysPlatformDiscoveryMatchesNativeFrontend" and .Action == "pass")
            ' "$HOME/build.test.json" >/dev/null
          ''}
          ${lib.optionalString (lib.elem "internal/transfer" testPackages) ''
            for required in \
              TestWholeIndexOptionalVariantExportMatchesNative \
              TestWholeIndexOptionalVariantExportMatchesNative/omitted-variant \
              TestWholeIndexOptionalVariantExportMatchesNative/different-real-architecture \
              TestWholeIndexOptionalVariantExportMatchesNative/omitted-platform-fields \
              TestSigningPreservesNativePasswordBytes/environment_multiline/stored \
              TestSigningPreservesNativePasswordBytes/environment_carriage_return/stored \
              TestSigningPreservesNativePasswordBytes/explicit_first_line/stored; do
              ${pkgs.jq}/bin/jq -es --arg required "$required" '
                any(.[]; .Test == $required and .Action == "pass")
              ' "$HOME/transfer.test.json" >/dev/null
            done
          ''}
          ${lib.optionalString withSequoia ''
            # Presence alone cannot satisfy the optional backend's live proof.
            for required in \
              TestSequoiaHelperSignsRegistryGraph \
              TestSequoiaHelperSignsNativeRoutes \
              TestSequoiaHelperSignsTransformedManifest \
              TestSequoiaHelperNativeDestinationBoundaries \
              TestSequoiaHelperDirectCopiesPreserveProtocolAndProtectSecrets \
              TestSequoiaHelperTransferCancellationCleansSecrets \
              TestSigningGapProtectedSequoiaGraph \
              TestSigningPreservesNativePasswordBytes \
              TestSigningGapFulcioOIDCRekor \
              TestSigningGapRejectsUntrustedOIDCToken \
              TestStoredIndexRegistryRetainsRootSignature \
              TestStoredIndexRegistryRetainsVerifiableSignatures \
              TestStoredSignatureActionsFollowNativeDestinations \
              TestLocalSignatureRemovalPreservesSiblingManifest \
              TestCopyIncompleteIndexDoesNotSilentlySelectOneMember \
              TestCopyPulledLeafRetainsOriginWithoutBecomingManifestList \
              TestCopyDecryptedIndexDistinguishesMissingMembers \
              TestStoredIndexPushHonorsNativePolicyAndImmutableRoot \
              TestStoredTransferOptionsReportsPasswordCleanupFailure; do
              ${pkgs.jq}/bin/jq -es --arg required "$required" '
                any(.[]; .Test == $required and .Action == "pass")
              ' "$HOME/transfer.test.json" >/dev/null
            done
          ''}
          ${lib.optionalString (group != null) ''
            ${pkgs.jq}/bin/jq -r '
              select(.Action == "pass" or .Action == "skip" or .Action == "fail")
              | select(.Test != null and (.Test | contains("/") | not))
              | .Test
            ' "$HOME/buildah.test.json" | sort > completed
            sort "$selected" > expected
            diff -u expected completed
            for required in \
              TestBuildPlanExecutesForeignBinaryThroughRegisteredBinfmt \
              TestRetainStoredLayoutLayersPreservesCompressionVariants \
              TestImportedExposeRootlessImage \
              TestNegativeCacheTTLMatchesNativeBuildah \
              TestWarmDeviceRunCacheStillRequiresEntitlement \
              TestBuildDefinitionUsesContainersConfigRuntimeDefaults \
              TestResolveImageSourcePinnedAlwaysUsesNativePolicy \
              TestWorkDirMetadataMatchesNativeBuildah \
              TestInheritedConfigurationAdmissionMatchesNativeBuildah \
              TestAggregateCachesInvalidateOldWorkdirSemantics \
              TestRemoteAddTLSPolicyMatchesNative \
              TestRemoteAddTLSBuildCacheAndEpoch \
              TestRemoteAddTLSCredentialRedirect \
              TestNetworkInterfaceUsesNativeConfiguration \
              TestDNSControlsMatchNativeParser \
              TestDNSConfiguredDefaultsStayNativeAndInvalidateCache \
              TestStoredTransferPassphrasesUseNativeFilesAndRawStagedSecrets \
              TestCacheGapAggregateRunAuthorizationAndFreshStoreReuse \
              TestCacheGapNamedContextWholeComponentReuse \
              TestCacheGapDynamicNestedAndDeferredGraphsReuseInstructions \
              TestCacheGapPackageAuthorizationBeforeFreshStoreReuse \
              TestCacheGapNamedContextContentInvalidation \
              TestCacheGapNestedWarmCacheStillChecksAuthorization \
              TestCacheGapNativeCommitRootMetadataComparator \
              TestCacheGapUnpinnedHTTPAddPortableReuseAndInvalidation \
              TestCacheGapUnpinnedGitAddPortableReuseAndInvalidation \
              TestCacheGapDynamicNestedAndDeferredPackageProducersReuseInstructions \
              TestCacheGapNativeDeviceCacheComparator \
              TestCacheGapDeviceSelectionAndAuthorizationAcrossCacheScopes \
              TestCacheGapOptionalDeviceAvailabilityInvalidatesPortableCache \
              TestCacheGapHostGatewayCacheMatchesNative \
              TestCacheGapConfiguredCDIDirectoryIgnoresBrokenDefaultSpecs; do
              if grep -qx "$required" "$selected"; then
                ${pkgs.jq}/bin/jq -es --arg required "$required" '
                  any(.[]; .Test == $required and .Action == "pass")
                ' "$HOME/buildah.test.json" >/dev/null
              fi
            done
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
          containers.storage.settings.storage = {
            graphroot = lib.mkForce "";
            runroot = lib.mkForce "";
          };
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
        environment.systemPackages =
          map (package: package.bin or package) runtime.nativeBuildInputs
          ++ [
            pkgs.diffutils
            pkgs.gnutar
            pkgs.python3
          ]
          ++ lib.optionals withSequoia [
            coopr-sequoia-signing
            pkgs.sequoia-sq
          ];
      };

      testScript = ''
        machine.wait_for_unit("default.target")
        machine.wait_for_unit("user@1000.service")
        machine.succeed("grep -qx enabled /proc/sys/fs/binfmt_misc/${foreignSystem}")
        machine.succeed("grep -q '^flags:.*F' /proc/sys/fs/binfmt_misc/${foreignSystem}")
        machine.succeed("su - coopr -c '${runner}' 2>&1 | tee /dev/ttyS0", timeout=85 * 60)
        ${lib.optionalString (builtins.elem "cmd/coopr" testPackages) ''
          machine.copy_from_machine("/home/coopr/coopr.test.json")
        ''}
        ${lib.optionalString (builtins.elem "internal/build" testPackages) ''
          machine.copy_from_machine("/home/coopr/build.test.json")
          machine.copy_from_machine("/home/coopr/library-proof.json")
        ''}
        ${lib.optionalString (builtins.elem "internal/buildah" testPackages) ''
          machine.copy_from_machine("/home/coopr/buildah.test.json")
        ''}
        ${lib.optionalString (builtins.elem "internal/transfer" testPackages) ''
          machine.copy_from_machine("/home/coopr/transfer.test.json")
        ''}
      '';
    };
in
{
  rootless-native = makeTest "coopr-rootless-native" {
    withSequoia = true;
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
  map (group: {
    name = "rootless-buildah-${group}";
    value = makeTest "coopr-rootless-buildah-${group}" { inherit group; };
  }) buildahGroupNames
)
