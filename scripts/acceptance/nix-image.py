#!/usr/bin/env python3
"""Qualify Nix inputs -> normal Coopr composition -> fresh Stoke Go tests.

Requires provisioned Nix and rootless Coopr runtime support. No host-Nix
executor, daemon socket mount, registry or VM is used. Source, cache and final
OCI data stay in a new private scratch directory; small logs stay in evidence.
"""

import argparse
import gzip
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import signal
import subprocess
import tempfile
import time


def require(condition, message):
    if not condition:
        raise AssertionError(message)


def write_json(path, value):
    path.write_text(json.dumps(value, indent=2) + "\n")


def fingerprint_source(root):
    files = {}
    paths = [root / name for name in ["go.mod", "go.sum", "LICENSE"]]
    for name in ["cmd", "internal"]:
        paths.extend(path for path in (root / name).rglob("*") if path.is_file())
    for path in sorted(paths):
        files[str(path.relative_to(root))] = hashlib.sha256(path.read_bytes()).hexdigest()
    encoded = json.dumps(files, sort_keys=True, separators=(",", ":")).encode()
    return {"sha256": hashlib.sha256(encoded).hexdigest(), "files": files}


def verify_layout(layout):
    def blob(descriptor):
        algorithm, encoded = descriptor["digest"].split(":", 1)
        path = layout / "blobs" / algorithm / encoded
        digest = hashlib.new(algorithm)
        with path.open("rb") as source:
            for chunk in iter(lambda: source.read(1024 * 1024), b""):
                digest.update(chunk)
        require(path.stat().st_size == descriptor["size"], "OCI blob size mismatch")
        require(digest.hexdigest() == encoded, "OCI blob digest mismatch")
        return path

    index = json.loads((layout / "index.json").read_text())
    require(len(index["manifests"]) == 1, "ambiguous final OCI layout")
    descriptor = index["manifests"][0]
    manifest = json.loads(blob(descriptor).read_text())
    config = json.loads(blob(manifest["config"]).read_text())
    for layer, expected in zip(manifest["layers"], config["rootfs"]["diff_ids"], strict=True):
        path = blob(layer)
        digest = hashlib.sha256()
        opener = gzip.open if layer["mediaType"].endswith("+gzip") else open
        with opener(path, "rb") as source:
            for chunk in iter(lambda: source.read(1024 * 1024), b""):
                digest.update(chunk)
        require("sha256:" + digest.hexdigest() == expected, "OCI DiffID mismatch")
    require(not any(path.is_symlink() for path in layout.rglob("*")), "external OCI symlink")
    return {"manifest": descriptor["digest"], "config": config}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--coopr-source", type=Path, required=True)
    parser.add_argument("--stoke-source", type=Path, required=True)
    parser.add_argument("--coopr-binary", type=Path, required=True)
    parser.add_argument("--stoke-binary", type=Path, required=True)
    parser.add_argument("--evidence", type=Path, required=True)
    parser.add_argument("--scratch-parent", type=Path, default=Path("/dev/shm"))
    parser.add_argument("--nix", default="nix")
    parser.add_argument("--runtime", default="/usr/bin/crun")
    parser.add_argument("--scratch-limit-gib", type=int, default=12,
                        help="maximum conservative scratch growth (default: 12 GiB)")
    parser.add_argument("--resume-fresh-tests", action="store_true",
                        help="reuse verified completed native phases and finish interrupted fresh tests")
    args = parser.parse_args()
    args.coopr_source = args.coopr_source.resolve(strict=True)
    args.stoke_source = args.stoke_source.resolve(strict=True)
    args.coopr_binary = args.coopr_binary.resolve(strict=True)
    args.stoke_binary = args.stoke_binary.resolve(strict=True)
    args.evidence.mkdir(parents=True, exist_ok=True, mode=0o700)
    args.evidence = args.evidence.resolve()
    require(1 <= args.scratch_limit_gib <= 16, "scratch limit must be between 1 and 16 GiB")
    if args.resume_fresh_tests:
        saved = json.loads((args.evidence / "scratch.json").read_text())
        scratch = Path(saved["path"])
        require(scratch.is_dir() and not scratch.is_symlink(), "saved scratch is unavailable")
        require(scratch.parent.resolve() == args.scratch_parent.resolve()
                and scratch.name.startswith("coopr-nix-go-")
                and scratch.stat().st_uid == saved["uid"] == os.getuid(), "scratch ownership mismatch")
    else:
        scratch = Path(tempfile.mkdtemp(prefix="coopr-nix-go-", dir=args.scratch_parent))
        scratch.chmod(0o700)
    initial_free = os.statvfs(scratch).f_bavail * os.statvfs(scratch).f_frsize
    source = fingerprint_source(args.stoke_source)
    if args.resume_fresh_tests:
        require(source == json.loads((args.evidence / "source.json").read_text()),
                "Stoke source differs from the completed phases")
        operations = json.loads((args.evidence / "operations.json").read_text())
        completed = {operation["label"] for operation in operations if operation["status"] == 0}
        require({"nix-prepare", "native-cold", "native-warm", "native-export", "native-independent"}
                <= completed, "resume requires all native phases already verified")
        initial_free += int(subprocess.run(["du", "-sb", str(scratch)], capture_output=True,
                                          text=True).stdout.split()[0])
    else:
        write_json(args.evidence / "scratch.json", {"path": str(scratch), "uid": os.getuid()})
        write_json(args.evidence / "source.json", source)
        operations = []
    size_measurements = []

    def size():
        # Native workers can temporarily own unreadable mountpoint directories.
        # Never change their permissions just to measure the test. The enclosing
        # filesystem's growth provides a conservative bound while du is partial.
        measured = subprocess.run(["du", "-sb", str(scratch)], capture_output=True, text=True)
        partial = int(measured.stdout.split()[0]) if measured.stdout.split() else 0
        current = os.statvfs(scratch)
        growth = max(0, initial_free - current.f_bavail * current.f_frsize)
        bounded = max(partial, growth)
        size_measurements.append({"bytes": bounded, "duBytes": partial,
                                  "duComplete": measured.returncode == 0,
                                  "enclosingFilesystemGrowth": growth})
        return bounded

    def run(label, command, env=None, timeout=600):
        require(not (args.evidence / (label + ".log")).exists(), "refusing to overwrite a prior log")
        started = time.monotonic()
        with (args.evidence / (label + ".log")).open("wb") as log:
            child = subprocess.Popen(command, env=env, stdout=log, stderr=subprocess.STDOUT,
                                     start_new_session=True)
            stopped = None
            try:
                while child.poll() is None:
                    try:
                        child.wait(timeout=5)
                    except subprocess.TimeoutExpired:
                        if time.monotonic() - started > timeout:
                            stopped = "timeout"
                        elif size() > args.scratch_limit_gib * 1024 ** 3:
                            stopped = f"scratch exceeded {args.scratch_limit_gib} GiB"
                        if stopped:
                            break
            finally:
                if child.poll() is None:
                    os.killpg(child.pid, signal.SIGTERM)
                    try:
                        child.wait(timeout=20)
                    except subprocess.TimeoutExpired:
                        os.killpg(child.pid, signal.SIGKILL)
                        child.wait()
        result = {"label": label, "command": list(map(str, command)), "status": child.returncode,
                  "seconds": time.monotonic() - started, "scratchBytes": size(), "stopped": stopped}
        operations.append(result)
        write_json(args.evidence / "operations.json", operations)
        require(child.returncode == 0, f"{label} failed: see {args.evidence / (label + '.log')}")
        return (args.evidence / (label + ".log")).read_text()

    coopr_input = json.dumps("path:" + str(args.coopr_source))
    stoke_input = json.dumps(str(args.stoke_source))
    require("${" not in coopr_input + stoke_input, "unsupported interpolation in source path")
    expression = f'''let
      source = builtins.getFlake {coopr_input};
      pkgs = import source.inputs.nixpkgs {{
        system = builtins.currentSystem;
        overlays = [ source.overlays.default ];
      }};
    in import (source.outPath + "/examples/nix/stoke-image.nix") {{
      inherit pkgs;
      stokeSource = builtins.toPath {stoke_input};
    }}'''
    if not args.resume_fresh_tests:
        (args.evidence / "image-expression.nix").write_text(expression + "\n")
        run("nix-prepare", [args.nix, "--max-jobs", "2", "--cores", "2", "build", "--impure",
                            "--expr", expression, "--out-link", str(args.evidence / "launcher"),
                            "--print-build-logs"], timeout=900)
    launchers = list((args.evidence / "launcher/bin").iterdir())
    require(len(launchers) == 1, "expected one composition launcher")
    launcher = launchers[0]
    policy = scratch / "policy.json"
    write_json(policy, {"default": [{"type": "reject"}], "transports": {
        transport: {"": [{"type": "insecureAcceptAnything"}]}
        for transport in ["oci", "oci-archive", "containers-storage"]}})
    conf = scratch / "containers.conf"
    conf.write_text('[engine]\nhelper_binaries_dir=["/usr/libexec/podman"]\n'
                    'runtime="crun"\n[engine.runtimes]\ncrun=[' + json.dumps(args.runtime) + ']\n')
    env = os.environ.copy()
    env.update({"COOPR_BINARY": str(args.coopr_binary), "CONTAINERS_CONF": str(conf)})
    for variable, name in [("TMPDIR", "tmp"), ("XDG_DATA_HOME", "data"),
                           ("XDG_CONFIG_HOME", "config"), ("XDG_STATE_HOME", "state"),
                           ("XDG_CACHE_HOME", "cache")]:
        path = scratch / name
        path.mkdir(exist_ok=args.resume_fresh_tests)
        env[variable] = str(path)
    portable = scratch / "portable-cache"

    def native(label, store, import_cache=False):
        command = [str(launcher), "--root", str(scratch / (store + "-graph")),
                   "--runroot", str(scratch / (store + "-run")), "--storage-driver", "vfs",
                   "--network=none", "--pull=never", "--timestamp", "1", "--runtime", args.runtime,
                   "--signature-policy", str(policy), "--metadata-file", str(args.evidence / (label + ".metadata.json")),
                   "--iidfile", str(args.evidence / (label + ".id")),
                   "--cache-to", "type=local,dest=" + str(portable)]
        if import_cache:
            command.extend(["--cache-from", "type=local,src=" + str(portable)])
        log = ((args.evidence / (label + ".log")).read_text() if args.resume_fresh_tests
               else run(label, command, env))
        metadata = json.loads((args.evidence / (label + ".metadata.json")).read_text())
        lookups = [event for event in metadata.get("coopr.operation.progress", [])
                   if event.get("instruction") == "run" and event.get("phase") == "lookup"
                   and event.get("state") == "completed"]
        return {"id": (args.evidence / (label + ".id")).read_text().strip(), "lookup": lookups,
                "componentCacheHit": bool(re.search(r"(?m)^--> Using component cache sha256:[0-9a-f]{64}\s*$", log)),
                "executed": bool(re.search(r"(?m)^COOPR_NIX_GO_BUILD_EXECUTED\s*$", log))}

    cold = native("native-cold", "primary")
    warm = native("native-warm", "primary", True)
    require(cold["executed"], "cold native RUN was not observed")
    require(not warm["executed"] and cold["id"] == warm["id"], "warm native output not reused exactly")
    require(warm["componentCacheHit"] or any(event.get("cache") == "hit" for event in warm["lookup"]),
            "no native warm cache hit")
    context = scratch / "stoke-context"
    context.mkdir(exist_ok=args.resume_fresh_tests)
    layout = context / "environment"
    coopr = [str(args.coopr_binary), "--root", str(scratch / "primary-graph"),
             "--runroot", str(scratch / "primary-run"), "--storage-driver", "vfs"]
    if not args.resume_fresh_tests:
        run("native-export", coopr + ["image", "push", cold["id"], "oci:" + str(layout) + ":image",
                                      "--signature-policy", str(policy), "--format", "oci"], env)
    exported = verify_layout(layout)
    config = exported["config"]
    require(config["config"]["WorkingDir"] == "/workspace", "native workdir not inherited")
    require("COOPR_ENVIRONMENT=nix-stoke" in config["config"]["Env"], "initial config not inherited")
    require(any(value.startswith("GO_COMMAND=/nix/store/") for value in config["config"]["Env"]),
            "argument-derived Go configuration missing")
    independent = native("native-independent", "independent", True)
    require(not independent["executed"] and independent["id"] == cold["id"],
            "independent OCI cache did not restore exact native result")
    require(independent["componentCacheHit"] or any(event.get("cache") == "hit" for event in independent["lookup"]),
            "independent OCI cache hit not observed")
    workflow = context / "stoke.yaml"
    shutil.copyfile(args.coopr_source / "examples/nix/stoke-go.yaml", workflow)
    reports = []
    for label in ["stoke-fresh-1", "stoke-fresh-2"]:
        if args.resume_fresh_tests and label in completed:
            log = (args.evidence / (label + ".log")).read_text()
        else:
            if args.resume_fresh_tests:
                attempt = 1
                while (args.evidence / (label + f"-retry-{attempt}.log")).exists():
                    attempt += 1
                label += f"-retry-{attempt}"
            log = run(label, [str(args.stoke_binary), "run", "-f", str(workflow), "-context", str(context),
                          "-coopr-binary", str(args.coopr_binary), "-cache-dir", str(scratch / "stoke-cache"),
                          "-output", str(scratch / label), "-signature-policy", str(policy),
                          "-runtime", args.runtime, "-storage-driver", "vfs"], env)
        require(re.search(r"(?m)^STOKE_NIX_FRESH_TESTS_PASSED\s*$", log), "actual fresh Go tests not observed")
        report = next(json.loads(line) for line in reversed(log.splitlines())
                      if line.startswith('{"status":'))
        require(report["status"] == "passed", "Stoke run failed")
        require(all(task.get("cache", {}).get("Hits", 0) == 0 for task in report["tasks"]),
                "fresh tests consumed an execution-cache hit")
        reports.append(report)
    require(source == fingerprint_source(args.stoke_source), "Stoke source changed during qualification")
    result = {"status": "PASS", "nativeCold": cold, "nativeWarm": warm,
              "nativeIndependent": independent, "finalOCI": exported, "freshStokeRuns": reports,
              "sourceSHA256": source["sha256"], "scratch": str(scratch), "scratchBytes": size(),
              "sizeMeasurements": size_measurements,
              "scratchLimitGiB": args.scratch_limit_gib,
              "hostNixExecutor": False, "containerFlakeCheckClaimed": False,
              "registryPublication": False, "runningOperations": []}
    write_json(args.evidence / "result.json", result)
    print(json.dumps({"status": result["status"], "evidence": str(args.evidence)}, indent=2))


if __name__ == "__main__":
    main()
