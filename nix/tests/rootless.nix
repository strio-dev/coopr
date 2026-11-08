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
      "internal/apibackend"
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
      pastaNetwork ? false,
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
        // lib.optionalAttrs (lib.elem "internal/apibackend" testPackages && !pastaNetwork) {
          COOPR_TEST_SERVICE_BINARY = "${coopr}/bin/coopr";
        }
        // lib.optionalAttrs pastaNetwork {
          COOPR_TEST_PASTA_REQUIRED = "1";
          COOPR_TEST_PASTA_HOST = "192.168.1.1";
          COOPR_TEST_PASTA_PACKAGE = "${pkgs.passt}";
          COOPR_TEST_PASTA_EVIDENCE = "/home/coopr/pasta-proof";
          GOMAXPROCS = "1";
          GOMEMLIMIT = "768MiB";
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
          ${lib.optionalString pastaNetwork ''
            selector='^TestPublicClientPastaNetworkWorkload$'
            mkdir -p "$COOPR_TEST_PASTA_EVIDENCE"
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
                  lib.optionalString (group != null || pastaNetwork) ''-test.run "$selector"''
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
          ${lib.optionalString (lib.elem "internal/apibackend" testPackages && !pastaNetwork) ''
            for required in \
              TestPublicClientNativeServiceLifetime \
              TestPublicClientNativeServiceLifetime/parent-eof \
              TestPublicClientNativeServiceLifetime/service-sigterm \
              TestPublicClientNativeServiceLifetime/owner-sigkill \
              TestPublicClientNativeLiterals \
              TestPublicClientDefinitionProgressColdWarm \
              TestAPIWorkersOverlapAndRecoverGraphCacheAcrossRestarts \
              TestAPICancellationDrainsWorkerBeforeAcknowledgingAndDoesNotCacheRun \
              TestNativeOperationProgressColdWarmAndFailure \
              TestNativeDefinitionMetadataContainsOperationTimings \
              TestNativeHandledFailureRetainsProgressTruncation \
              TestPublicClientNativeConcurrentCacheInvalidationAndIntegrity \
              TestPublicClientNativeCancellationWaitsForWorkerDrain \
              TestPublicClientNativeBuildAndConcurrentPublication \
              TestPublicClientBaleNestedPackageRelocationWithoutSourceRegistry; do
              ${pkgs.jq}/bin/jq -es --arg required "$required" '
                any(.[]; .Test == $required and .Action == "pass")
              ' "$HOME/apibackend.test.json" >/dev/null
            done
          ''}
          ${lib.optionalString pastaNetwork ''
            # A top-level pass can contain skipped children. Require every
            # traffic/freshness/cancellation variant to have actually passed.
            for required in \
              TestPublicClientPastaNetworkWorkload \
              TestPublicClientPastaNetworkWorkload/serial \
              TestPublicClientPastaNetworkWorkload/serial/fresh-1 \
              TestPublicClientPastaNetworkWorkload/serial/fresh-2 \
              TestPublicClientPastaNetworkWorkload/serial/cancel \
              TestPublicClientPastaNetworkWorkload/isolated-stage \
              TestPublicClientPastaNetworkWorkload/isolated-stage/fresh-1 \
              TestPublicClientPastaNetworkWorkload/isolated-stage/fresh-2 \
              TestPublicClientPastaNetworkWorkload/isolated-stage/cancel; do
              ${pkgs.jq}/bin/jq -es --arg required "$required" '
                any(.[]; .Test == $required and .Action == "pass")
              ' "$HOME/apibackend.test.json" >/dev/null
            done
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
      globalTimeout = (if pastaNetwork then 15 else 90) * 60;

      nodes.machine = {
        imports = lib.optionals pastaNetwork [
          {
            # Pasta copies the primary address. The peer/resolver lives on a
            # separate VLAN while eth0 retains the ordinary QEMU NAT route.
            networking.interfaces.eth0.useDHCP = true;
            networking.nameservers = lib.mkForce [ "192.168.1.1" ];
            networking.dhcpcd.extraConfig = "nohook resolv.conf";
            networking.resolvconf.enable = false;
            networking.useHostResolvConf = false;
            networking.firewall.allowedUDPPorts = [ 53 ];
            networking.firewall.allowedTCPPorts = [
              53
              18081
            ];
            environment.etc."resolv.conf" = {
              text = "nameserver 192.168.1.1\n";
              mode = "direct-symlink";
            };
            services.resolved.enable = false;
            services.dnsmasq = {
              enable = true;
              resolveLocalQueries = false;
              settings = {
                bind-interfaces = true;
                listen-address = [ "192.168.1.1" ];
                no-resolv = true;
                no-hosts = true;
                local = "/pasta-proof.test/";
                address = "/pasta-proof.test/192.168.1.1";
                local-ttl = 0;
                log-queries = true;
                log-facility = "-";
              };
            };
          }
        ];
        virtualisation = {
          memorySize = if pastaNetwork then 2304 else 4096;
          diskSize = if pastaNetwork then 8192 else 16384;
          cores = if pastaNetwork then 1 else 4;
          podman.enable = true;
          containers.storage.settings.storage = {
            graphroot = lib.mkForce "";
            runroot = lib.mkForce "";
          };
          containers.containersConf.settings.engine.helper_binaries_dir = [
            "${pkgs.netavark}/bin"
            "${pkgs.aardvark-dns}/bin"
          ]
          ++ lib.optionals pastaNetwork [ "${pkgs.passt}/bin" ];
          additionalPaths = [ runTests ];
        }
        // lib.optionalAttrs pastaNetwork { vlans = [ 1 ]; };
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
        ${
          if pastaNetwork then
            ''
              import datetime as dt
              import json
              import shlex

              machine.wait_for_unit("dnsmasq.service")
              machine.wait_for_open_port(53, addr="192.168.1.1", timeout=dt.timedelta(seconds=60))
              machine.wait_until_succeeds("ip -4 route show default | grep -q 'via 10.0.2.2 dev eth0'", timeout=dt.timedelta(seconds=60))
              routes = json.loads(machine.succeed("ip -j -4 route get 1.1.1.1"))
              assert routes[0]["dev"] == "eth0" and routes[0].get("prefsrc", routes[0].get("src")) != "192.168.1.1", routes
              resolver = machine.succeed("cat /etc/resolv.conf")
              assert [line.split()[1] for line in resolver.splitlines() if line.startswith("nameserver ")] == ["192.168.1.1"], resolver
              assert "pasta-proof.test" not in machine.succeed("cat /etc/hosts").lower()
              machine.succeed("install -d -m0700 -o coopr -g users /home/coopr/pasta-proof")
              observations = {}
              for key, command in {
                  "osRelease": "cat /etc/os-release",
                  "kernel": "uname -a",
                  "invokingUser": "su - coopr -c id",
                  "pastaVersion": "${pkgs.passt}/bin/pasta --version",
                  "pastaResolvedPath": "readlink -f ${pkgs.passt}/bin/pasta",
                  "pastaPackageHash": "sha256sum ${pkgs.passt}/bin/pasta",
                  "selinuxEnforce": "if test -r /sys/fs/selinux/enforce; then cat /sys/fs/selinux/enforce; else printf 'unavailable\\n'; fi",
                  "securityLabels": "ls -LdZ / ${pkgs.passt}/bin/pasta",
                  "routes": "ip -j -4 route show",
                  "addresses": "ip -j -4 addr show",
                  "resolver": "cat /etc/resolv.conf",
                  "backendTestHash": "sha256sum ${integrationTests}/bin/apibackend.test",
              }.items():
                  status, output = machine.execute(command)
                  observations[key] = {"exit": status, "output": output}
              machine.succeed("printf '%s' " + shlex.quote(json.dumps(observations, indent=2)) + " > /home/coopr/pasta-environment.json")
              try:
                  machine.succeed("bash -o pipefail -c " + shlex.quote("su - coopr -c '${runner}' 2>&1 | tee /dev/ttyS0"), timeout=10 * 60)
                  messages = [json.loads(line).get("MESSAGE", "") for line in machine.succeed("journalctl -u dnsmasq --no-pager -o json").splitlines()]
                  for variant in ["serial", "isolated-stage"]:
                      hostname = variant + ".pasta-proof.test"
                      queries = [message for message in messages if "query[A]" in message and hostname in message]
                      assert len(queries) >= 3, (hostname, queries)
                      result = json.loads(machine.succeed("cat /home/coopr/pasta-proof/" + variant + ".json"))
                      assert result["status"] == "PASS" and result["httpRequests"] == 3 and result["freshResponses"] == 2 and result["cancelledConnectionClosed"], result
                      assert len(result["runs"]) == 3 and all(run["cleanupBeforeAcknowledgement"] for run in result["runs"]), result
              finally:
                  machine.execute("journalctl -u dnsmasq --no-pager > /home/coopr/pasta-dnsmasq.log")
                  for path in ["/home/coopr/pasta-environment.json", "/home/coopr/apibackend.test.json", "/home/coopr/pasta-dnsmasq.log", "/home/coopr/pasta-proof/serial.json", "/home/coopr/pasta-proof/isolated-stage.json"]:
                      if machine.execute("test -f " + shlex.quote(path))[0] == 0:
                          machine.copy_from_machine(path)
            ''
          else
            ''
              import shlex
              machine.succeed("bash -o pipefail -c " + shlex.quote("su - coopr -c '${runner}' 2>&1 | tee /dev/ttyS0"), timeout=85 * 60)
            ''
        }
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
        ${lib.optionalString (builtins.elem "internal/apibackend" testPackages && !pastaNetwork) ''
          machine.copy_from_machine("/home/coopr/apibackend.test.json")
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
      "internal/apibackend"
      "internal/transfer"
    ];
  };
  rootless-packaged = makeTest "coopr-rootless-packaged" { acceptance = "dynamic"; };
  rootless-static = makeTest "coopr-rootless-static" { acceptance = "static"; };
  rootless-pasta = makeTest "coopr-rootless-pasta" {
    packages = [ "internal/apibackend" ];
    pastaNetwork = true;
  };
}
// lib.listToAttrs (
  map (group: {
    name = "rootless-buildah-${group}";
    value = makeTest "coopr-rootless-buildah-${group}" { inherit group; };
  }) buildahGroupNames
)
