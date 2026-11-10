{ pkgs }:
let
  inherit (pkgs) lib;
  digest = "sha256:${lib.concatStrings (lib.replicate 64 "a")}";
  literal = "quote ' and \" and \${caller} and $(printf injected)\\tail\nsecond line";
  fakeCoopr = pkgs.writeScriptBin "coopr" ''
    #!${pkgs.python3}/bin/python3
    import json, os, signal, sys, time
    with open(os.environ["COOPR_TEST_CALLS"], "a") as output:
        output.write(json.dumps(sys.argv[1:]) + "\n")
    if os.environ.get("COOPR_TEST_READY"):
        def stop(number, frame):
            sys.exit(128 + number)
        for number in [signal.SIGINT, signal.SIGTERM]:
            signal.signal(number, stop)
        ready = os.environ["COOPR_TEST_READY"]
        with open(ready + ".tmp", "w") as output:
            output.write(str(os.getpid()))
        # File existence signals readiness only after the complete PID is visible.
        os.replace(ready + ".tmp", ready)
        while True:
            time.sleep(0.01)
    if sys.argv[1:3] == ["component", "inspect"]:
        print(json.dumps({"digest": "${digest}"}))
    else:
        print("${digest}")
    sys.exit(int(os.environ.get("COOPR_TEST_STATUS", "0")))
  '';
  serializer = {
    buildLayer = options: (pkgs.writeText "component-unit-layer" "{}") // { unitOptions = options; };
    buildImage =
      options:
      (pkgs.writeText "component-unit-image" "{}")
      // {
        unitOptions = options;
      };
  };
  buildComponent = import ../build-component.nix {
    inherit pkgs;
    coopr = fakeCoopr;
    nix2container = serializer;
    exporter = pkgs.writeShellScriptBin "coopr-nix-export" ''
      mkdir -p "$2"
      printf '%s' '{"imageLayoutVersion":"1.0.0"}' > "$2/oci-layout"
    '';
  };
  generated = buildComponent {
    name = "unit-component";
    tag = "dev";
    components = [
      {
        ref = "registry.example/child@${digest}";
        args.message = literal;
        args.tool = "${pkgs.hello}/bin/hello";
      }
    ];
  };
  archived = buildComponent {
    name = "unit-archive-parent";
    components =
      map
        (suffix: {
          ref = "registry.example/child@${digest}";
          archive = pkgs.writeText "child-${suffix}.tar" suffix;
          bundle = pkgs.emptyDirectory;
          args.message = "selected-${suffix}";
        })
        [
          "a"
          "b"
        ];
  };
  definition = pkgs.writeText "native-component.coopr" ''
    package as="files" { copy "payload" "/payload"; }
    extend as="selected" { copy "/payload" "/payload" from="files"; }
  '';
  context = pkgs.emptyDirectory;
  custom = buildComponent {
    name = "unit-custom";
    inherit definition context;
    target = "selected";
    args.message = literal;
  };
  selectedBinary =
    (import ../build-component.nix {
      inherit pkgs;
      coopr = throw "explicit cooprBinary must not force a default engine build";
      nix2container = serializer;
      exporter = pkgs.writeShellScriptBin "coopr-nix-export" ''
        mkdir -p "$2"
      '';
    })
      {
        name = "selected-engine";
        cooprBinary = "${fakeCoopr}/bin/coopr";
      };
  rejected =
    options:
    !(builtins.tryEval (
      builtins.deepSeq (buildComponent ({ name = "invalid"; } // options)).drvPath true
    )).success;
in
assert rejected { config.Env = [ "LOST=value" ]; };
assert rejected { cooprBinary = "/usr/bin/coopr"; };
assert builtins.deepSeq selectedBinary.drvPath true;
assert builtins.deepSeq archived.drvPath true;
assert rejected {
  components = [
    {
      ref = "registry.example/child@${digest}";
      bundle = pkgs.emptyDirectory;
    }
  ];
};
assert rejected { components = [ { ref = "registry.example/child:mutable"; } ]; };
assert rejected { args.message = literal; };
assert rejected { target = "not-declared"; };
assert rejected { definition = "/tmp/mutable-definition"; };
assert rejected {
  inherit definition;
  components = [ { ref = digest; } ];
};
assert generated.artifact == "component";
assert builtins.length generated.inputImage.unitOptions.layers == 1;
pkgs.runCommand "coopr-nix-component-unit-check"
  {
    nativeBuildInputs = [ pkgs.python3 ];
    generatedProgram = lib.getExe generated;
    archivedProgram = lib.getExe archived;
    archivedRecipe = archived.recipe;
    archivedDefinition = archived.definition;
    generatedDefinition = generated.definition;
    generatedRecipe = generated.recipe;
    customProgram = lib.getExe custom;
    customRecipe = custom.recipe;
    expectedLiteral = literal;
    expectedDefinition = definition;
    expectedContext = context;
  }
  ''
    mkdir "$out"
    python3 - <<'PY'
    import json, os, signal, subprocess, time
    from pathlib import Path

    root = Path(os.environ["TMPDIR"]) / "component-unit"
    root.mkdir()
    generated = json.loads(Path(os.environ["generatedRecipe"]).read_text())
    custom = json.loads(Path(os.environ["customRecipe"]).read_text())
    archived = json.loads(Path(os.environ["archivedRecipe"]).read_text())
    definition = Path(os.environ["generatedDefinition"]).read_text()
    assert 'from "coopr-nix" as="coopr-nix-input"' in definition
    assert 'package as="coopr-nix-closure"' in definition
    assert 'copy "/" "/" from="coopr-nix-closure"' in definition
    assert "extend {" in definition and "run {" not in definition
    assert "env " not in definition and "workdir " not in definition
    assert generated["args"] == {}
    assert 'component "registry.example/child@${digest}"' in definition
    escaped = os.environ["expectedLiteral"].replace("\\", "\\\\").replace("$", "\\$")
    assert "arg \"message\" " + json.dumps(escaped, ensure_ascii=False) in definition
    assert "COOPR_NIX_COMPONENT_" not in definition
    assert custom["definition"] == os.environ["expectedDefinition"]
    assert custom["context"] == os.environ["expectedContext"]

    def environment(label):
        directory = root / label
        directory.mkdir()
        env = os.environ.copy()
        env["COOPR_TEST_CALLS"] = str(directory / "calls.jsonl")
        return directory, env

    def invoke(label, flags=(), program="generatedProgram", expected=0, extra=None):
        directory, env = environment(label)
        env.update(extra or {})
        result = subprocess.run([os.environ[program], *flags], cwd=root, env=env, capture_output=True, text=True, timeout=10)
        assert result.returncode == expected, (label, result.returncode, result.stderr)
        path = directory / "calls.jsonl"
        calls = [json.loads(line) for line in path.read_text().splitlines()] if path.exists() else []
        return calls

    flags = ["--network=none", "--cache-to=registry.example/cache", "--metadata-file=result.json"]
    calls = invoke("generated", flags)
    assert len(calls) == 1 and calls[0][:2] == ["component", "build"]
    call = calls[0]
    assert call[call.index("--file") + 1] == generated["definition"]
    assert call[call.index("--platform") + 1] == generated["platform"]
    assert "coopr-nix=oci-layout://" + generated["input"] + ":image" in call
    assert call[call.index("--tag") + 1] == "unit-component:dev"
    assert call[-len(flags):] == flags
    assert call.index(generated["context"]) < call.index(flags[0])
    archive_flags = ["--bundle-output=archive-parent.bale", "--metadata-file=archive-parent.json"]
    archive_calls = invoke("archive-selected", archive_flags, program="archivedProgram")
    assert [call[:2] for call in archive_calls] == [["component", "load"], ["component", "inspect"], ["component", "load"], ["component", "inspect"], ["component", "build"]]
    selected = archived["components"]
    assert len(selected) == 2 and selected[0]["archiveName"] != selected[1]["archiveName"]
    archived_definition = Path(os.environ["archivedDefinition"]).read_text()
    for component in selected:
        assert component["archiveName"].startswith("coopr.invalid/nix/")
        assert 'component "' + component["invocationReference"] + '"' in archived_definition
        assert component["invocationReference"] + "=" + component["archiveName"] in archive_calls[-1]
    assert archive_calls[-1].count("--input-component") == 2
    assert archive_calls[-1].count("--input-bundle") == 1
    assert selected[0]["bundle"] in archive_calls[-1]
    assert archive_calls[-1][-len(archive_flags):] == archive_flags
    custom_call = invoke("custom", program="customProgram")[0]
    assert custom_call[:2] == ["component", "build"]
    assert custom_call[custom_call.index("--target") + 1] == "selected"
    assert "message=" + os.environ["expectedLiteral"] in custom_call
    assert "--tag" not in custom_call
    for number, flags in enumerate([["--file=other"], ["--from=other"], ["--platform=linux/arm64"],
                                    ["--build-context=coopr-nix=/other"], ["--"], ["-qfother"]]):
        assert invoke("managed-" + str(number), flags, expected=2) == []
    assert invoke("arg", ["--build-arg=message=override"], program="customProgram", expected=2) == []
    invoke("failure", expected=19, extra={"COOPR_TEST_STATUS": "19"})
    assert invoke("missing", expected=2, extra={"COOPR_BINARY": "/unavailable/coopr"}) == []

    for number in [signal.SIGINT, signal.SIGTERM]:
        directory, env = environment("signal-" + str(number))
        ready = directory / "ready"
        env["COOPR_TEST_READY"] = str(ready)
        process = subprocess.Popen([os.environ["generatedProgram"]], cwd=root, env=env, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        deadline = time.monotonic() + 5
        while not ready.exists() and process.poll() is None and time.monotonic() < deadline:
            time.sleep(0.01)
        assert ready.exists(), process.communicate(timeout=1)
        assert int(ready.read_text()) == process.pid
        process.send_signal(number)
        stdout, stderr = process.communicate(timeout=5)
        assert process.returncode == 128 + number, (process.returncode, stderr)

    Path(os.environ["out"], "result.json").write_text(json.dumps({
        "status": "PASS", "nativeComponentCommand": True, "literalBindingEncoding": True,
        "configurationNotTranslated": True, "archiveSpecificCoordinates": True, "nativeBundleOptions": True, "nativeExecutionQualified": False
    }, indent=2) + "\n")
    PY
  ''
