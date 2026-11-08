{ pkgs }:
let
  inherit (pkgs) lib;
  digestA = "sha256:${lib.concatStrings (lib.replicate 64 "a")}";
  digestB = "sha256:${lib.concatStrings (lib.replicate 64 "b")}";
  # These are launcher unit tests. Real serialization and native execution are
  # qualified separately; the fake CLI records exact argv and process lifetime.
  fakeCoopr = pkgs.writeTextFile {
    name = "coopr-nix-unit-cli";
    destination = "/bin/coopr";
    executable = true;
    text = ''
      #!${pkgs.python3}/bin/python3
      import json, os, signal, sys, time
      arguments = sys.argv[1:]
      with open(os.environ["COOPR_TEST_CALLS"], "a") as output:
          output.write(json.dumps(arguments) + "\n")
      kind = arguments[1] if arguments[:2] in [["component", "load"], ["component", "inspect"]] else "build"
      if os.environ.get("COOPR_TEST_BLOCK") == kind:
          def stop(number, frame):
              with open(os.environ["COOPR_TEST_STOPPED"], "w") as output:
                  output.write(str(number))
              sys.exit(128 + number)
          for number in [signal.SIGTERM, signal.SIGINT, signal.SIGHUP]:
              signal.signal(number, stop)
          ready = os.environ["COOPR_TEST_READY"]
          with open(ready + ".tmp", "w") as output:
              output.write(str(os.getpid()))
          # File existence signals readiness only after the complete PID is visible.
          os.replace(ready + ".tmp", ready)
          while True:
              time.sleep(0.02)
      if kind == "load":
          if os.environ.get("COOPR_TEST_LOAD_STATUS"):
              sys.exit(int(os.environ["COOPR_TEST_LOAD_STATUS"]))
          # Match the real tagged-load CLI: stdout is the supplied tag alone.
          print(os.environ.get("COOPR_TEST_LOAD_STDOUT", arguments[arguments.index("--tag") + 1]))
      elif kind == "inspect":
          if os.environ.get("COOPR_TEST_INSPECT_STATUS"):
              sys.exit(int(os.environ["COOPR_TEST_INSPECT_STATUS"]))
          descriptor = {"mediaType": "application/vnd.oci.image.manifest.v1+json", "digest": os.environ.get("COOPR_TEST_INSPECT_DIGEST", "${digestA}"), "size": 1}
          print(os.environ.get("COOPR_TEST_INSPECT_JSON", json.dumps(descriptor)))
      else:
          # Model the native parser's missing-value error for this regression.
          # A context appended after --label would instead be consumed as value.
          if arguments[-1:] == ["--label"]:
              sys.exit(2)
          sys.exit(int(os.environ.get("COOPR_TEST_STATUS", "0")))
    '';
  };
  stubSerializer = {
    buildLayer =
      options:
      (pkgs.writeText "coopr-nix-unit-layer" (builtins.toJSON options))
      // {
        unitOptions = options;
      };
    buildImage =
      options:
      (pkgs.writeText "coopr-nix-unit-image.json" "{}")
      // {
        unitOptions = options;
      };
  };
  buildImage = import ../build-image.nix {
    inherit pkgs;
    coopr = fakeCoopr;
    nix2container = stubSerializer;
    exporter = pkgs.writeShellScriptBin "coopr-nix-export" ''
      mkdir -p "$2"
      printf '%s' '{"imageLayoutVersion":"1.0.0"}' > "$2/oci-layout"
    '';
  };
  archive = pkgs.writeText "coopr-nix-unit-archive" "unit fixture, not an OCI archive";
  alternateArchive = pkgs.writeText "coopr-nix-unit-archive-alternate" "another delivery fixture, not an OCI archive";
  literal = "quote ' and \" and \${literal} and $(printf injected)\nsecond line";
  generated = buildImage {
    name = "unit-generated";
    tag = "dev";
    components = [
      {
        ref = "registry.example/first@${digestA}";
        inherit archive;
        args.message = literal;
      }
      {
        ref = "registry.example/alternate@${digestA}";
        archive = alternateArchive;
      }
      {
        ref = "registry.example/second@${digestB}";
        args.other = "two";
      }
    ];
  };
  nativeDefinition = pkgs.writeText "coopr-nix-unit-native.coopr" ''
    from "coopr-nix" as="image" { arg "message"; }
  '';
  callerContext = pkgs.runCommand "coopr-nix-unit-context" { } ''
    mkdir "$out"
    echo declared > "$out/caller-input"
  '';
  native = buildImage {
    name = "unit-native";
    definition = nativeDefinition;
    context = callerContext;
    target = "image";
    args.message = literal;
  };
  plain = buildImage { name = "unit-plain"; };
  argumentOnly = buildImage {
    name = "unit-argument-closure";
    components = [
      {
        ref = digestA;
        args.tool = "${pkgs.hello}/bin/hello";
      }
    ];
  };
  forgedHostInput = builtins.appendContext "/tmp/coopr-nix-mutable-input" (
    builtins.getContext "${callerContext}"
  );
  escapingInput = "${callerContext}/../../../tmp/coopr-nix-mutable-input";
  rejectedInput =
    input:
    rejected { definition = input; }
    && rejected { context = input; }
    && rejected {
      components = [
        {
          ref = digestA;
          archive = input;
        }
      ];
    };
  rejected =
    options:
    !(builtins.tryEval (builtins.deepSeq (buildImage ({ name = "invalid"; } // options)).drvPath true))
    .success;
in
assert rejected { components = [ { ref = "registry.example/component:mutable"; } ]; };
assert rejected {
  config.Healthcheck = {
    Test = [
      "CMD"
      "ignored"
    ];
  };
};
assert rejected {
  definition = nativeDefinition;
  components = [ { ref = digestA; } ];
};
assert rejected {
  components = [
    {
      ref = digestA;
      args.bad-name = "value";
    }
  ];
};
assert rejected {
  components = [
    {
      ref = digestA;
      args.message = 1;
    }
  ];
};
assert rejected {
  components = [
    {
      ref = digestA;
      typo = "value";
    }
  ];
};
assert rejected { args.message = "no native definition"; };
assert rejected { target = "no native definition"; };
assert rejectedInput forgedHostInput;
assert rejectedInput escapingInput;
assert !(rejected { context = "${callerContext}/declared-subdirectory"; });
assert !(rejected { definition = ../../examples/nix/environment.coopr; });
assert !(generated ? oci);
assert generated.inputImage.unitOptions.layers == [ ];
assert builtins.length argumentOnly.inputImage.unitOptions.layers == 1;
assert (
  let
    layer = builtins.head argumentOnly.inputImage.unitOptions.layers;
  in
  toString layer.unitOptions.ignore == toString (builtins.head layer.unitOptions.deps)
);
pkgs.runCommand "coopr-nix-composition-unit-check"
  {
    nativeBuildInputs = [ pkgs.python3 ];
    generatedProgram = lib.getExe generated;
    nativeProgram = lib.getExe native;
    plainProgram = lib.getExe plain;
    plainRecipe = plain.recipe;
    generatedDefinition = generated.definition;
    recipe = generated.recipe;
    nativeRecipe = native.recipe;
    expectedLiteral = literal;
    expectedDigest = digestA;
    expectedNativeDefinition = nativeDefinition;
    expectedContext = callerContext;
    passthru = { inherit generated native plain; };
  }
  ''
    mkdir "$out"
    python3 - <<'PY'
    import hashlib, json, os, signal, subprocess, time
    from pathlib import Path

    root = Path(os.environ["TMPDIR"]) / "launcher-unit"
    root.mkdir()
    recipe = json.loads(Path(os.environ["recipe"]).read_text())
    native_recipe = json.loads(Path(os.environ["nativeRecipe"]).read_text())
    definition = Path(os.environ["generatedDefinition"]).read_text()
    for name in recipe["args"]:
        assert definition.count('arg ' + json.dumps(name)) == 2
        assert chr(36) + '{' + name + '}' in definition
    archived = [component for component in recipe["components"] if component["archive"] is not None]
    assert len(archived) == 2
    assert archived[0]["digest"] == archived[1]["digest"] == os.environ["expectedDigest"]
    assert archived[0]["archive"] != archived[1]["archive"]
    assert archived[0]["archiveName"] != archived[1]["archiveName"]
    for component in archived:
        expected_name = "coopr-nix-" + hashlib.sha256(component["archive"].encode()).hexdigest() + ":archive"
        assert component["archiveName"] == expected_name
        assert component["invocationReference"] == expected_name + "@" + component["digest"]
        assert component["ref"].startswith("registry.example/")
        assert 'component ' + json.dumps(component["invocationReference"]) in definition
    remote = recipe["components"][-1]
    assert remote["archive"] is None and remote["archiveName"] is None
    assert remote["invocationReference"] == remote["ref"]
    assert definition.index(archived[0]["invocationReference"]) < definition.index(archived[1]["invocationReference"]) < definition.index(remote["ref"])
    assert native_recipe["definition"] == os.environ["expectedNativeDefinition"]
    assert native_recipe["context"] == os.environ["expectedContext"]
    assert not (Path(recipe["context"]) / "implicit-cwd-input").exists()
    (root / "implicit-cwd-input").write_text("must not enter primary context")

    def environment(label, additions=None):
        directory = root / label
        directory.mkdir()
        result = os.environ.copy()
        result.update({"TMPDIR": str(directory), "COOPR_TEST_CALLS": str(directory / "calls.jsonl")})
        if additions:
            result.update(additions)
        return directory, result

    def calls(directory):
        path = directory / "calls.jsonl"
        return [json.loads(line) for line in path.read_text().splitlines()] if path.exists() else []

    def invoke(label, flags=(), program="generatedProgram", additions=None, expected=0):
        directory, env = environment(label, additions)
        result = subprocess.run([os.environ[program], *flags], cwd=root, env=env, capture_output=True, text=True, timeout=10)
        assert result.returncode == expected, (label, result.returncode, result.stderr)
        assert not list(directory.glob("coopr-nix-load.*")), label
        return directory, calls(directory)

    forwarded = ["--cache-from", "registry.example/cache", "--cache-to=registry.example/cache", "--signature-policy=/runtime/policy", "--root", "/runtime/store", "--build-context", "caller=/runtime/context", "--build-arg", "OTHER=runtime value", "--label=--platform=literal"]
    directory, recorded = invoke("missing-label-value", ["--label"], program="plainProgram", expected=2)
    plain_recipe = json.loads(Path(os.environ["plainRecipe"]).read_text())
    assert recorded[0][-1] == "--label"
    assert recorded[0].index(plain_recipe["context"]) < recorded[0].index("--label")
    directory, recorded = invoke("forwarded", forwarded)
    assert len(recorded) == 5
    for component, offset in zip(archived, [0, 2]):
        load, inspect = recorded[offset:offset + 2]
        assert load == ["component", "load", "--tag", component["archiveName"], "--input", component["archive"]]
        assert inspect == ["component", "inspect", component["archiveName"]]
    build = recorded[4]
    assert build[0] == "build" and build.index(recipe["context"]) < build.index("--cache-from")
    assert build[build.index("--file") + 1] == recipe["definition"]
    assert build[build.index("--platform") + 1] == recipe["platform"]
    assert "coopr-nix=oci-layout://" + recipe["input"] + ":image" in build
    assert build[build.index("--tag") + 1] == "unit-generated:dev"
    assert build[build.index("--cache-from"):build.index("--cache-from") + len(forwarded)] == forwarded
    for key, value in recipe["args"].items():
        assert key + "=" + value in build
    assert os.environ["expectedLiteral"] in recipe["args"].values()

    directory, recorded = invoke("native", program="nativeProgram")
    assert len(recorded) == 1
    assert recorded[0][recorded[0].index("--target") + 1] == "image"
    assert recorded[0][-1] == os.environ["expectedContext"]
    assert "message=" + os.environ["expectedLiteral"] in recorded[0]
    assert "--tag" not in recorded[0]

    for i, flags in enumerate([
        ["--platform=linux/arm64"], ["--platform", "linux/arm64"], ["--arch=arm64"],
        ["--os=linux"], ["--variant=v8"], ["--all-platforms"], ["-fother"],
        ["--file", "other"], ["--from=other"], ["--build-context", "COOPR-NIX=/other"],
        ["--build-context=coopr-nix=/other"], ["--build-arg", "COOPR_NIX_COMPONENT_0_0=override"],
        ["--build-arg=COOPR_NIX_COMPONENT_0_0"], ["--"],
      ["-qfother"], ["-qf", "other"],
    ]):
        directory, recorded = invoke("protected-" + str(i), flags, expected=2)
        assert recorded == [], (flags, recorded)
    directory, recorded = invoke("target", ["--target=other"], program="nativeProgram", expected=2)
    assert recorded == []

    directory, recorded = invoke("mismatch", additions={"COOPR_TEST_INSPECT_DIGEST": "sha256:" + "b" * 64}, expected=2)
    assert recorded == [["component", "load", "--tag", archived[0]["archiveName"], "--input", archived[0]["archive"]], ["component", "inspect", archived[0]["archiveName"]]]
    directory, recorded = invoke("malformed-digest", additions={"COOPR_TEST_INSPECT_DIGEST": "mutable-name"}, expected=2)
    assert len(recorded) == 2
    directory, recorded = invoke("malformed-descriptor", additions={"COOPR_TEST_INSPECT_JSON": "not JSON"}, expected=2)
    assert len(recorded) == 2
    directory, recorded = invoke("ambiguous-descriptor", additions={"COOPR_TEST_INSPECT_JSON": '{"digest":"' + os.environ["expectedDigest"] + '"}\n{"digest":"' + os.environ["expectedDigest"] + '"}'}, expected=2)
    assert len(recorded) == 2
    directory, recorded = invoke("load-stdout-not-identity", additions={"COOPR_TEST_LOAD_STDOUT": "log line\nwrong@sha256:" + "b" * 64})
    assert len(recorded) == 5
    invoke("load-failure", additions={"COOPR_TEST_LOAD_STATUS": "17"}, expected=17)
    directory, recorded = invoke("inspect-failure", additions={"COOPR_TEST_INSPECT_STATUS": "23"}, expected=23)
    assert len(recorded) == 2
    invoke("build-failure", program="nativeProgram", additions={"COOPR_TEST_STATUS": "19"}, expected=19)
    directory, recorded = invoke("missing-binary", additions={"COOPR_BINARY": "/unavailable/coopr"}, expected=2)
    assert recorded == []
    attack = root / "not-created"
    directory, recorded = invoke("literal-runtime-name", ["--build-arg", "$(touch " + str(attack) + ")=value"])
    assert not attack.exists()

    for kind, program in [("load", "generatedProgram"), ("inspect", "generatedProgram"), ("build", "nativeProgram")]:
        for number in [signal.SIGTERM, signal.SIGINT]:
            directory, env = environment(kind + "-signal-" + str(number), {"COOPR_TEST_BLOCK": kind})
            ready = directory / "ready"
            stopped = directory / "stopped"
            env.update({"COOPR_TEST_READY": str(ready), "COOPR_TEST_STOPPED": str(stopped)})
            process = subprocess.Popen([os.environ[program]], env=env, cwd=root, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
            deadline = time.monotonic() + 5
            while not ready.exists() and process.poll() is None and time.monotonic() < deadline:
                time.sleep(0.01)
            assert ready.exists(), (kind, process.communicate(timeout=1))
            child = int(ready.read_text())
            assert (child == process.pid) == (kind == "build"), (kind, child, process.pid)
            process.send_signal(number)
            stdout, stderr = process.communicate(timeout=5)
            assert process.returncode == 128 + number, (kind, number, process.returncode, stderr)
            assert int(stopped.read_text()) == number
            assert not list(directory.glob("coopr-nix-load.*"))
            try:
                os.kill(child, 0)
            except ProcessLookupError:
                pass
            else:
                raise AssertionError((kind, "child survived cancellation", child))
            assert len(calls(directory)) == (2 if kind == "inspect" else 1)

    (Path(os.environ["out"]) / "inputs.json").write_text(json.dumps({
        key: os.environ[key] for key in ["generatedProgram", "nativeProgram", "plainProgram", "plainRecipe", "generatedDefinition", "recipe", "nativeRecipe"]
    }, indent=2) + "\n")
    print("launcher unit tests passed: argv, pins, contexts, precedence, exit status and PID-only cancellation")
    PY
  ''
