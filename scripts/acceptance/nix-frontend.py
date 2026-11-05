#!/usr/bin/env python3
"""Qualify the Nix launcher through ordinary native Coopr, outside Nix builds.

Requires a configured rootless Coopr runtime and Nix. All native stores are
private test fixtures. Nothing is pushed and existing user stores are untouched.
"""

import argparse
import gzip
import hashlib
import io
import json
import os
from pathlib import Path
import re
import signal
import subprocess
import tarfile
import tempfile
import time


def json_write(path, value):
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(value, indent=2) + "\n")


def file_hash(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


class Qualification:
    def __init__(self, options):
        self.options = options
        self.repo = options.repo.resolve()
        self.coopr = options.coopr_binary.resolve()
        self.evidence = options.evidence.resolve()
        if self.evidence.exists() and any(self.evidence.iterdir()):
            raise ValueError("evidence directory must be new or empty")
        self.evidence.mkdir(parents=True, exist_ok=True)
        self.scratch = Path(tempfile.mkdtemp(prefix="coopr-nix-frontend-", dir=options.scratch_parent))
        self.scratch.chmod(0o700)
        self.context = self.scratch / "source"
        self.context.mkdir()
        self.operations = []
        self.results = {}
        self.phases = {}
        self.active_process = None
        self.nix = [options.nix_binary, "--extra-experimental-features", "nix-command flakes"]
        source = Path(__file__).read_bytes()
        (self.evidence / "harness.py").write_bytes(source)
        json_write(self.evidence / "ownership.json", {
            "scratch": str(self.scratch), "uid": os.getuid(),
            "purpose": "private native acceptance fixtures", "repo": str(self.repo),
            "coopr": str(self.coopr), "cooprSHA256": file_hash(self.coopr),
            "harnessSHA256": hashlib.sha256(source).hexdigest(),
        })

    def environment(self, name):
        if name in self.phases:
            return self.phases[name]
        root = self.scratch / name
        root.mkdir()
        env = os.environ.copy()
        for variable, directory in [("TMPDIR", "tmp"), ("XDG_DATA_HOME", "data"),
                                    ("XDG_CONFIG_HOME", "config"), ("XDG_CACHE_HOME", "cache"),
                                    ("XDG_STATE_HOME", "state")]:
            env[variable] = str(root / directory)
            (root / directory).mkdir()
        env["COOPR_BINARY"] = str(self.coopr)
        policy = root / "policy.json"
        json_write(policy, {"default": [{"type": "reject"}], "transports": {
            transport: {"": [{"type": "insecureAcceptAnything"}]}
            for transport in ["oci", "oci-archive", "containers-storage"]
        }})
        conf = root / "containers.conf"
        helpers = self.options.helper_directory or ["/usr/libexec/podman"]
        conf.write_text('[engine]\nhelper_binaries_dir=' + json.dumps(helpers)
                        + '\nruntime="crun"\n[engine.runtimes]\ncrun='
                        + json.dumps([self.options.runtime]) + '\n')
        env["CONTAINERS_CONF"] = str(conf)
        value = (root, env, policy)
        self.phases[name] = value
        return value

    def run(self, label, command, phase="primary", expect=0, timeout=None):
        _, env, _ = self.environment(phase)
        started = time.monotonic()
        log = self.evidence / (label + ".log")
        with log.open("wb") as output:
            process = subprocess.Popen([str(x) for x in command], cwd=self.context, env=env,
                                       stdout=output, stderr=subprocess.STDOUT, start_new_session=True)
            self.active_process = process
            try:
                status = process.wait(timeout=timeout or self.options.timeout)
            except subprocess.TimeoutExpired:
                os.killpg(process.pid, signal.SIGTERM)
                try:
                    process.wait(timeout=15)
                except subprocess.TimeoutExpired:
                    os.killpg(process.pid, signal.SIGKILL)
                    process.wait()
                raise RuntimeError(f"{label}: timeout; see {log}")
        self.active_process = None
        self.operations.append({"label": label, "command": [str(x) for x in command],
                                "phase": phase, "status": status,
                                "seconds": time.monotonic() - started, "log": str(log)})
        json_write(self.evidence / "operations.json", self.operations)
        if expect is not None and status != expect:
            raise RuntimeError(f"{label}: exit {status}, expected {expect}:\n{log.read_text()[-3000:]}")
        self.check_size()
        return status, log.read_text()

    def stop_active(self):
        """Stop only the process/session this harness launched, then drain it."""
        process = self.active_process
        if process is None or process.poll() is not None:
            return
        process.send_signal(signal.SIGTERM)
        try:
            process.wait(timeout=20)
        except subprocess.TimeoutExpired:
            os.killpg(process.pid, signal.SIGKILL)
            process.wait(timeout=10)
        self.active_process = None

    def native(self, phase="primary"):
        root, _, _ = self.environment(phase)
        return [str(self.coopr), "--root", str(root / "graph"), "--runroot", str(root / "run"),
                "--storage-driver", "vfs"]

    def build_options(self, label, phase="primary"):
        root, _, policy = self.environment(phase)
        return ["--root", str(root / "graph"), "--runroot", str(root / "run"),
                "--storage-driver", "vfs", "--runtime", self.options.runtime,
                "--network=none", "--pull=never", "--timestamp", "1",
                "--signature-policy", str(policy), "--iidfile", str(self.evidence / (label + ".iid")),
                "--metadata-file", str(self.evidence / (label + ".metadata.json"))]

    def component(self, name, definition):
        source = self.context / (name + ".coopr")
        source.write_text(definition)
        _, _, policy = self.environment("primary")
        _, text = self.run(name + "-component", self.native() + ["component", "build", str(source),
                          "--network=none", "--pull=never", "--timestamp", "1",
                          "--signature-policy", str(policy)])
        roots = re.findall(r"(?m)^(sha256:[a-f0-9]{64})\s*$", text)
        if not roots:
            raise RuntimeError("component build did not return an immutable root")
        digest = roots[-1]
        archive = self.context / (name + ".oci.tar")
        self.run(name + "-save", self.native() + ["component", "save", digest, "--output", str(archive)])
        (self.evidence / source.name).write_text(definition)
        json_write(self.evidence / (name + "-artifact.json"), {
            "digest": digest, "archive": str(archive), "archiveSHA256": file_hash(archive),
        })
        return digest, archive

    def declaration(self, name, component, message, replacement=False,
                    config_revision=None, content_revision=None):
        message_file = self.context / (name + "-message.json")
        json_write(message_file, message)
        revision_file = self.context / (name + "-revision.json")
        json_write(revision_file, content_revision)
        roots = "[ ]" if content_revision is None else (
            '[ (pkgs.writeTextDir "input-revision" (builtins.fromJSON '
            '(builtins.readFile ' + str(revision_file) + '))) ]')
        extra_environment = [] if config_revision is None else ["INPUT_CONFIG_REV=" + config_revision]
        component_declaration = "[ ]"
        if component is not None:
            digest, archive = component
            arguments = "{ }" if replacement else (
                '{ message = builtins.fromJSON (builtins.readFile ' + str(message_file)
                + '); tool = "${pkgs.hello}/bin/hello"; }')
            component_declaration = "[{ ref = " + json.dumps(digest) + "; archive = " + str(archive)
            component_declaration += "; args = " + arguments + "; }]"
        expression = self.context / (name + ".nix")
        expression.write_text('''let
  source = builtins.getFlake %s;
  pkgs = import source.inputs.nixpkgs {
    system = builtins.currentSystem;
    overlays = [ source.overlays.default ];
  };
in pkgs.coopr.buildImage {
  name = "coopr-nix-frontend-test";
  packages = [ pkgs.bash pkgs.coreutils ];
  copyToRoot = %s;
  config = {
    Env = [ "PATH=/bin" "SOURCE_MARK=from-nix" ] ++ builtins.fromJSON %s;
    WorkingDir = "/";
    Entrypoint = [ "${pkgs.bash}/bin/bash" ];
  };
  components = %s;
}
''' % (json.dumps("path:" + str(self.repo)), roots, json.dumps(json.dumps(extra_environment)),
       component_declaration))
        (self.evidence / expression.name).write_text(expression.read_text())
        _, text = self.run(name + "-prepare", self.nix + ["build", "--impure", "--file", str(expression),
                          "--out-link", str(self.evidence / (name + "-launcher")), "--print-out-paths"])
        outputs = [line for line in text.splitlines() if line.startswith("/nix/store/") and Path(line).is_dir()]
        if not outputs:
            raise RuntimeError("Nix returned no launcher output")
        package = Path(outputs[-1])
        launchers = list((package / "bin").iterdir())
        if len(launchers) != 1:
            raise RuntimeError("ambiguous launcher output")
        _, recipe_path = self.run(name + "-recipe", self.nix + ["eval", "--impure", "--raw", "--expr",
                                 "(import " + str(expression) + ").recipe"])
        # Nix may emit evaluation warnings before the raw path.
        recipes = [Path(line) for line in recipe_path.splitlines()
                   if line.startswith("/nix/store/") and Path(line).is_file()]
        if not recipes:
            raise RuntimeError("Nix returned no recipe")
        recipe = json.loads(recipes[-1].read_text())
        json_write(self.evidence / (name + ".recipe.json"), recipe)
        return launchers[0], recipe

    def compose(self, label, app, phase="primary", extra=None):
        status, text = self.run(label, [app] + self.build_options(label, phase) + (extra or []), phase)
        image = (self.evidence / (label + ".iid")).read_text().strip()
        metadata = json.loads((self.evidence / (label + ".metadata.json")).read_text())
        events = [event for event in metadata.get("coopr.operation.progress", [])
                  if event.get("instruction") == "run" and event.get("phase") == "lookup"
                  and event.get("state") == "completed"]
        result = {"imageID": image, "runLookupEvents": events,
                  "executionMarker": "FRONTEND_RUN_EXECUTED\n" in text,
                  "componentCacheHit": "--> Using component cache " in text,
                  "status": status}
        self.results[label] = result
        return result

    def export(self, label, image, phase="primary"):
        root, _, policy = self.environment(phase)
        layout = root / (label + "-oci")
        self.run(label + "-export", self.native(phase) + ["image", "push", image,
                 "oci:" + str(layout) + ":image", "--signature-policy", str(policy), "--format", "oci"], phase)
        return self.inspect(layout)

    @staticmethod
    def inspect(layout):
        def blob(descriptor):
            data = (layout / "blobs" / descriptor["digest"].replace(":", "/")).read_bytes()
            assert len(data) == descriptor["size"]
            assert "sha256:" + hashlib.sha256(data).hexdigest() == descriptor["digest"]
            return data
        index = json.loads((layout / "index.json").read_text())
        assert len(index["manifests"]) == 1
        manifest = json.loads(blob(index["manifests"][0]))
        config = json.loads(blob(manifest["config"]))
        selected = {}
        for descriptor, diff_id in zip(manifest["layers"], config["rootfs"]["diff_ids"], strict=True):
            data = blob(descriptor)
            raw = gzip.decompress(data) if descriptor["mediaType"].endswith("+gzip") else data
            assert "sha256:" + hashlib.sha256(raw).hexdigest() == diff_id
            with tarfile.open(fileobj=io.BytesIO(raw), mode="r:") as archive:
                for member in archive:
                    name = member.name.removeprefix("./").lstrip("/")
                    if member.isfile() and name in ["proof", "nonce", "hello", "input-revision"]:
                        selected[name] = archive.extractfile(member).read().decode()
        return {"manifest": index["manifests"][0]["digest"], "config": config, "files": selected}

    def check_size(self):
        total = sum(path.stat().st_size for path in self.scratch.rglob("*")
                    if path.is_file() and not path.is_symlink())
        if total > 2 * 1024**3:
            raise RuntimeError(f"private runtime exceeded 2 GiB bound: {total}")
        return total

    @staticmethod
    def require_cache(result, expected):
        # Portable restoration can reuse the complete component before entering
        # its RUN graph. In that case native Coopr logs the component-cache hit
        # rather than emitting a RUN lookup event. Output/nonce checks below
        # independently distinguish that reuse from reexecution.
        hit = expected == "hit" and result["componentCacheHit"]
        assert hit or any(event.get("cache") == expected for event in result["runLookupEvents"]), result

    def cancellation(self, app):
        label = "cancel"
        _, env, _ = self.environment("primary")
        command = [str(app)] + self.build_options(label)
        log = self.evidence / (label + ".log")
        with log.open("wb") as output:
            process = subprocess.Popen(command, cwd=self.context, env=env, stdout=output,
                                       stderr=subprocess.STDOUT, start_new_session=True)
            self.active_process = process
            deadline = time.monotonic() + 45
            while time.monotonic() < deadline:
                if "FRONTEND_SLEEP_STARTED\n" in log.read_text():
                    break
                if process.poll() is not None:
                    raise RuntimeError("cancellation fixture terminated before readiness")
                time.sleep(0.1)
            else:
                os.killpg(process.pid, signal.SIGTERM)
                process.wait(timeout=15)
                raise RuntimeError("cancellation fixture never reached RUN")
            # Signal the launcher PID only. It has exec'd the ordinary Coopr CLI.
            os.kill(process.pid, signal.SIGTERM)
            try:
                status = process.wait(timeout=20)
            except subprocess.TimeoutExpired:
                os.killpg(process.pid, signal.SIGTERM)
                process.wait(timeout=15)
                raise RuntimeError("PID-only cancellation did not drain Coopr")
        self.active_process = None
        assert status != 0
        assert not (self.evidence / (label + ".iid")).exists()
        # All workers inherit the fixture temp directory in their request paths.
        survivors = []
        prefix = str(self.scratch).encode()
        for entry in Path("/proc").iterdir():
            if not entry.name.isdigit() or int(entry.name) == os.getpid():
                continue
            try:
                command_line = (entry / "cmdline").read_bytes()
            except (FileNotFoundError, PermissionError, ProcessLookupError):
                continue
            if prefix in command_line and b"coopr" in command_line:
                survivors.append(int(entry.name))
        assert not survivors, survivors
        self.results[label] = {"status": status, "pidOnlySignal": "SIGTERM",
                               "readinessObserved": True, "ownedWorkersRemaining": survivors}

    def delivery_bindings(self, payload):
        """Exercise valid Bale archives sharing a root but not a delivery."""
        if self.options.delivery_archive_a is None:
            return
        cases = [
            ("delivery-a", self.options.delivery_archive_a, self.options.delivery_repository_a),
            ("delivery-b", self.options.delivery_archive_b, self.options.delivery_repository_b),
        ]
        delivered = []
        for label, archive, repository in cases:
            app, recipe = self.declaration(label, (payload[0], archive.resolve()), label)
            result = self.compose(label, app)
            image = self.export(label, result["imageID"])
            assert image["files"]["proof"] == label
            component = recipe["components"][0]
            reference = component["invocationReference"]
            assert reference == component["archiveName"] + "@" + payload[0]
            delivered.append((app, reference, repository))

        status, text = self.run("delivery-bare-inspect", self.native()
                                + ["component", "inspect", payload[0]], expect=None)
        assert status != 0 and "multiple delivery contexts" in text
        bindings = []
        for index, (_app, reference, repository) in enumerate(delivered):
            _, text = self.run("delivery-qualified-" + str(index), self.native()
                               + ["component", "inspect", reference])
            descriptor = json.loads(text)
            assert descriptor["digest"] == payload[0]
            binding = json.loads(descriptor["annotations"]["dev.coopr.strio.component.binding"])
            assert binding["repository"] == repository
            bindings.append({"reference": reference, "binding": binding})
        assert bindings[0]["reference"] != bindings[1]["reference"]
        assert bindings[0]["binding"] != bindings[1]["binding"]
        again = self.compose("delivery-a-after-b", delivered[0][0])
        self.require_cache(again, "hit")
        assert not again["executionMarker"]
        assert self.export("delivery-a-after-b", again["imageID"])["files"]["proof"] == "delivery-a"
        self.results["archiveDeliveryBindings"] = {
            "sameComponentRoot": payload[0], "bareDigestAmbiguous": True,
            "qualifiedBindings": bindings, "aAfterBPasses": True,
        }

    def execute(self):
        plain_app, plain_recipe = self.declaration("plain", None, "unused")
        assert plain_recipe["args"] == {} and plain_recipe["components"] == []
        status, text = self.run("missing-value", [plain_app] + self.build_options("missing-value")
                                + ["--label"], expect=None)
        assert status != 0 and "flag needs an argument: --label" in text
        assert "STEP " not in text
        assert not (self.evidence / "missing-value.iid").exists()
        self.results["missingNativeOptionValueRejectedBeforeBuild"] = True
        script = ('test "$SOURCE_MARK" = from-nix; test -x "$TOOL_ARG"; '
                  '"$TOOL_ARG" > /hello; printf %s "$RESULT_MESSAGE" > /proof; '
                  'head -c 16 /dev/urandom | base64 > /nonce; echo FRONTEND_RUN_EXECUTED')
        payload = self.component("payload", 'extend {\n arg "message" "default"\n'
                                 ' arg "tool" "/bin/false"\n env RESULT_MESSAGE="${message}" TOOL_ARG="${tool}"\n'
                                 ' run { exec "/bin/bash" "-euc" ' + json.dumps(script) + '; }\n}\n')
        message = 'literal $value "quotes" \'apostrophe\' $(not-a-command)\nlast line\n'
        app, recipe = self.declaration("cold", payload, message)
        input_image = self.inspect(Path(recipe["input"]))
        assert "proof" not in input_image["files"]
        cache = self.scratch / "portable-cache"
        cold = self.compose("cold", app, extra=["--cache-to", "type=local,dest=" + str(cache)])
        self.require_cache(cold, "miss")
        assert cold["executionMarker"]
        cold_image = self.export("cold", cold["imageID"])
        assert cold_image["files"]["proof"] == message
        assert cold_image["files"]["hello"] == "Hello, world!\n"
        config = cold_image["config"]
        assert config["config"]["WorkingDir"] == "/"
        assert config["config"]["Entrypoint"] == input_image["config"]["config"]["Entrypoint"]
        assert "SOURCE_MARK=from-nix" in config["config"]["Env"]
        assert config["os"] == "linux" and config["architecture"] == recipe["platform"].split("/")[1]
        warm = self.compose("warm", app)
        self.require_cache(warm, "hit")
        assert not warm["executionMarker"] and warm["imageID"] == cold["imageID"]

        # Invoke the exact generated native recipe directly, bypassing launcher.
        direct_options = self.build_options("direct")
        direct = self.native() + ["build", "--file", recipe["definition"], "--platform", recipe["platform"],
                  "--build-context", "coopr-nix=oci-layout://" + recipe["input"] + ":image"]
        # Global native options are accepted after the subcommand too.
        direct += direct_options
        for key, value in sorted(recipe["args"].items()):
            direct += ["--build-arg", key + "=" + value]
        direct += [recipe["context"]]
        self.run("direct", direct)
        assert (self.evidence / "direct.iid").read_text().strip() == cold["imageID"]

        changed_app, _ = self.declaration("changed", payload, "changed argument")
        changed = self.compose("changed", changed_app)
        self.require_cache(changed, "miss")
        assert changed["executionMarker"] and changed["imageID"] != cold["imageID"]
        changed_image = self.export("changed", changed["imageID"])
        assert changed_image["files"]["proof"] == "changed argument"
        assert changed_image["files"]["nonce"] != cold_image["files"]["nonce"]

        config_app, _ = self.declaration("config-change", payload, message, config_revision="config-v2")
        config_result = self.compose("config-change", config_app)
        self.require_cache(config_result, "miss")
        config_image = self.export("config-change", config_result["imageID"])
        assert config_result["executionMarker"]
        assert "INPUT_CONFIG_REV=config-v2" in config_image["config"]["config"]["Env"]
        assert config_image["files"]["nonce"] != cold_image["files"]["nonce"]
        content_app, _ = self.declaration("content-change", payload, message, content_revision="content-v2")
        content_result = self.compose("content-change", content_app)
        self.require_cache(content_result, "miss")
        content_image = self.export("content-change", content_result["imageID"])
        assert content_result["executionMarker"]
        assert content_image["files"]["input-revision"] == "content-v2"
        assert content_image["files"]["nonce"] != cold_image["files"]["nonce"]

        independent = self.compose("restored", app, "independent", ["--cache-from", "type=local,src=" + str(cache)])
        self.require_cache(independent, "hit")
        assert not independent["executionMarker"]
        assert independent["imageID"] == cold["imageID"]
        independent_image = self.export("restored", independent["imageID"], "independent")
        assert independent_image["files"] == cold_image["files"]
        assert independent_image["config"]["config"] == cold_image["config"]["config"]
        empty = self.compose("empty-cache", app, "empty-cache")
        self.require_cache(empty, "miss")
        assert empty["executionMarker"]
        empty_image = self.export("empty-cache", empty["imageID"], "empty-cache")
        assert empty_image["files"]["proof"] == message
        assert empty_image["files"]["nonce"] != cold_image["files"]["nonce"]
        assert empty["imageID"] != cold["imageID"]

        wrong = ("sha256:" + "0" * 64, payload[1])
        mismatch_app, _ = self.declaration("mismatch", wrong, "never executed")
        status, text = self.run("mismatch", [mismatch_app] + self.build_options("mismatch"), expect=None)
        assert status != 0 and "differs from requested" in text
        assert not (self.evidence / "mismatch.iid").exists()

        self.delivery_bindings(payload)

        replacement = self.component("replacement", 'extend {\n from "scratch" {\n'
                                     ' env FINAL_FROM="independent"\n workdir "/different"\n }\n}\n')
        replacement_app, _ = self.declaration("replacement", replacement, "unused", replacement=True)
        replacement_result = self.compose("replacement", replacement_app)
        final_config = self.export("replacement", replacement_result["imageID"])["config"]["config"]
        assert final_config["WorkingDir"] == "/different"
        assert "FINAL_FROM=independent" in final_config["Env"]
        assert "SOURCE_MARK=from-nix" not in final_config["Env"]
        assert not final_config.get("Entrypoint")
        scratch_definition = self.context / "scratch-control.coopr"
        scratch_definition.write_text('from "scratch" {\n env FINAL_FROM="independent"\n'
                                      ' workdir "/different"\n}\n')
        self.run("scratch-control", self.native() + ["build", str(scratch_definition)]
                 + self.build_options("scratch-control"))
        scratch_id = (self.evidence / "scratch-control.iid").read_text().strip()
        scratch_config = self.export("scratch-control", scratch_id)["config"]["config"]
        assert final_config == scratch_config

        sleeper = self.component("sleeper", 'extend {\n run { exec "/bin/bash" "-euc" '
                                 + json.dumps('echo FRONTEND_SLEEP_STARTED; sleep 30; echo must-not-complete > /late')
                                 + '; }\n}\n')
        sleeper_app, _ = self.declaration("sleeper", sleeper, "unused", replacement=True)
        self.cancellation(sleeper_app)
        recovered = self.compose("after-cancel", app)
        self.require_cache(recovered, "hit")
        assert recovered["imageID"] == cold["imageID"]
        self.results["directNativeEquivalent"] = True
        self.results["argumentOnlyPackageClosureExecuted"] = True
        self.results["literalMessagePreserved"] = True
        self.results["independentFromConfigPreserved"] = True
        self.results["independentFromNativeConfigEquivalent"] = True
        self.results["configurationAndContentChangesInvalidate"] = True
        self.results["initialInputConfig"] = input_image["config"]["config"]
        self.results["coldFinalConfig"] = config["config"]
        self.results["coldFinalManifest"] = cold_image["manifest"]
        self.results["scratchBytes"] = self.check_size()
        self.results["status"] = "PASS"
        self.results["runningOwnedOperations"] = []
        self.results["hostNixExecutor"] = False
        self.results["cooprExecutedInsideNixSandbox"] = False
        json_write(self.evidence / "result.json", self.results)
        return self.results


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--repo", type=Path, default=Path(__file__).resolve().parents[2])
    parser.add_argument("--coopr-binary", type=Path, required=True)
    parser.add_argument("--evidence", type=Path, required=True)
    parser.add_argument("--scratch-parent", type=Path, default=Path("/dev/shm"))
    parser.add_argument("--nix-binary", default="nix")
    parser.add_argument("--runtime", default="/usr/bin/crun")
    parser.add_argument("--helper-directory", action="append",
                        help="native netavark/aardvark helper directory (repeatable)")
    parser.add_argument("--delivery-archive-a", type=Path,
                        help="optional public-Bale fixture A for this payload component")
    parser.add_argument("--delivery-archive-b", type=Path,
                        help="paired same-root public-Bale fixture B")
    parser.add_argument("--delivery-repository-a", default="delivery.example/bundle-a/a")
    parser.add_argument("--delivery-repository-b", default="delivery.example/bundle-b/b")
    parser.add_argument("--timeout", type=int, default=180)
    options = parser.parse_args()
    if (options.delivery_archive_a is None) != (options.delivery_archive_b is None):
        parser.error("delivery archives must be supplied as a pair")
    qualification = Qualification(options)

    def interrupted(signum, _frame):
        # Unwind wait() before cleanup, rather than entering wait recursively
        # from a signal handler while its internal lock may be held.
        raise InterruptedError(f"acceptance interrupted by signal {signum}")

    signal.signal(signal.SIGTERM, interrupted)
    signal.signal(signal.SIGHUP, interrupted)
    try:
        result = qualification.execute()
    except BaseException as error:
        qualification.stop_active()
        json_write(qualification.evidence / "failure.json", {
            "status": "FAILED", "error": repr(error), "scratch": str(qualification.scratch),
            "operations": qualification.operations, "results": qualification.results,
        })
        raise
    print(json.dumps(result, indent=2))


if __name__ == "__main__":
    main()
