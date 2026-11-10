#!/usr/bin/env python3
"""Qualify pkgs.coopr.buildComponent through native outside-sandbox packaging.

Requires provisioned Nix and ordinary rootless Coopr. Stores and source contexts
are private fixtures; results are retained and successful scratch is removed. Complete delivery
uses only a disposable loopback registry and performs no external publication.
"""

import argparse
import importlib.util
import json
import os
from pathlib import Path
import re
import shutil
import signal
import sys
import tarfile


spec = importlib.util.spec_from_file_location("coopr_nix_frontend", Path(__file__).with_name("nix-frontend.py"))
frontend = importlib.util.module_from_spec(spec)
spec.loader.exec_module(frontend)


nix_string = frontend.nix_string


class Qualification(frontend.Qualification):
    def __init__(self, options):
        super().__init__(options)
        source = Path(__file__).read_bytes()
        (self.evidence / "component-harness.py").write_bytes(source)
        ownership_path = self.evidence / "ownership.json"
        ownership = json.loads(ownership_path.read_text())
        ownership["componentHarnessSHA256"] = frontend.file_hash(Path(__file__))
        frontend.json_write(ownership_path, ownership)

    def declaration_component(self, label, child=None, message="", definition=None, args=None, archive=None, bundle=None):
        expression = self.scratch / (label + ".nix")
        children = "[]" if child is None else '''[
          { ref = %s; %s args = { message = %s; tool = "${pkgs.hello}/bin/hello"; }; }
        ]''' % (nix_string(child),
               ("archive = builtins.path { path = builtins.toPath " + nix_string(archive) + '; name = "child.oci.tar"; }; ' if archive is not None else "")
               + ("bundle = builtins.path { path = builtins.toPath " + nix_string(bundle) + '; name = "child.bale"; }; ' if bundle is not None else ""),
               nix_string(message))
        native = "" if definition is None else (
            "definition = builtins.path { path = builtins.toPath " + nix_string(definition)
            + '; name = "native-component.coopr"; };')
        arguments = "" if args is None else "args = builtins.fromJSON " + nix_string(json.dumps(args)) + ";"
        expression.write_text('''let
  source = builtins.getFlake %s;
  pkgs = import source.inputs.nixpkgs {
    system = %s;
    overlays = [ source.overlays.default ];
  };
in pkgs.coopr.buildComponent {
  name = %s;
  cooprBinary = builtins.path { path = builtins.toPath %s; name = "coopr-development"; };
  packages = [ pkgs.bash pkgs.coreutils %s ];
  components = %s;
  %s
  %s
}
''' % (nix_string("path:" + str(self.repo)), nix_string(self.options.system), nix_string(label), nix_string(self.coopr),
       "pkgs.hello" if child is None else "", children, native, arguments))
        return self.prepare(label, expression)

    def empty_image(self, label, child=None):
        expression = self.scratch / (label + ".nix")
        children = "[]" if child is None else "[{ ref = " + nix_string(child) + "; }]"
        expression.write_text('''let
  source = builtins.getFlake %s;
  pkgs = import source.inputs.nixpkgs {
    system = %s;
    overlays = [ source.overlays.default ];
  };
in pkgs.coopr.buildImage {
  name = %s;
  cooprBinary = builtins.path { path = builtins.toPath %s; name = "coopr-development"; };
  components = %s;
  config = { Env = [ "EMPTY_LITERAL=$value" ]; Labels."dev.strio.coopr.empty" = "preserved"; };
}
''' % (nix_string("path:" + str(self.repo)), nix_string(self.options.system), nix_string(label), nix_string(self.coopr), children))
        program, _ = self.prepare(label, expression, with_recipe=False)
        cold = self.compose(label + "-cold", program)
        warm = self.compose(label + "-warm", program)
        assert cold["imageID"] == warm["imageID"]
        view = self.export(label, cold["imageID"])
        config = view["config"]
        assert config["rootfs"]["type"] == "layers"
        assert "EMPTY_LITERAL=$value" in config["config"]["Env"]
        assert config["config"]["Labels"]["dev.strio.coopr.empty"] == "preserved"
        if child is not None:
            assert "EMPTY_CHILD=retained" in config["config"]["Env"]
            assert config["config"]["WorkingDir"] == "/child-config"
        self.results[label] = {"image": view, "cold": cold, "warm": warm,
                               "nixPackages": [], "nativeImportAndConstructor": True}

    def produce(self, label, program, bundle_output=None):
        root, _, policy = self.environment("primary")
        metadata_path = self.evidence / (label + ".metadata.json")
        extra = ["--bundle-output", bundle_output] if bundle_output is not None else []
        _, text = self.run(label, [program, "--root", root / "graph", "--runroot", root / "run",
                          "--storage-driver=vfs", "--runtime", self.options.runtime,
                          "--signature-policy", policy, "--network=none", "--pull=never", "--timestamp=1",
                          "--metadata-file", metadata_path] + extra)
        metadata = json.loads(metadata_path.read_text())
        digest = metadata.get("containerimage.digest", "")
        if metadata.get("coopr.outputError") or not re.fullmatch(r"sha256:[a-f0-9]{64}", digest):
            raise RuntimeError("component launcher returned no successful immutable native result")
        if bundle_output is not None:
            result = metadata.get("coopr.bundle", {})
            if result.get("error") or result.get("path") != str(bundle_output) or not result.get("descriptor", {}).get("digest"):
                raise RuntimeError("component launcher did not capture the complete bundle")
        archive = self.evidence / (label + ".oci.tar")
        self.run(label + "-save", self.native() + ["component", "save", digest, "--output", archive])
        with tarfile.open(archive) as saved:
            members = {member.name.removeprefix("./"): member for member in saved}

            def blob(descriptor):
                data = saved.extractfile(members["blobs/" + descriptor["digest"].replace(":", "/")]).read()
                assert len(data) == descriptor["size"]
                assert frontend.hashlib.sha256(data).hexdigest() == descriptor["digest"].removeprefix("sha256:")
                return json.loads(data)

            index = json.load(saved.extractfile(members["index.json"]))
            pending = [entry for entry in index["manifests"] if entry["digest"] == digest]
            assert len(pending) == 1
            packages = []
            while pending:
                document = blob(pending.pop())
                if "manifests" in document:
                    pending.extend(document["manifests"])
                elif document.get("artifactType") == "application/vnd.coopr.strio.dev.component.v1+json":
                    value = blob(document["config"])
                    assert value["packages"]
                    # Residual definitions preserve original FROM slots for
                    # dynamic invocation choices. Qualify the captured PACKAGE
                    # snapshots here and their independent replay in consume().
                    assert len(value["packages"]) == len(document["layers"])
                    for package, layer in zip(value["packages"], document["layers"]):
                        descriptor = package["descriptor"]
                        assert descriptor == layer
                        assert descriptor["mediaType"] == "application/vnd.coopr.strio.dev.package.v1+tar"
                        rootfs = package["config"]["rootfs"]
                        assert rootfs["type"] == "layers" and rootfs["diff_ids"] == [descriptor["digest"]]
                        member = members["blobs/" + descriptor["digest"].replace(":", "/")]
                        assert member.size == descriptor["size"]
                        with saved.extractfile(member) as snapshot:
                            assert frontend.hashlib.file_digest(snapshot, "sha256").hexdigest() == descriptor["digest"].removeprefix("sha256:")
                    packages.append(value)
            assert len(packages) == 1
        frontend.json_write(self.evidence / (label + ".published-component.json"), packages[0])
        assert "NIX_COMPONENT_CHILD_RAN\n" not in text
        assert "NIX_CUSTOM_COMPONENT_RAN\n" not in text
        return digest, archive

    def consume(self, label, artifact, child_archive=None, message=None, recipe=None):
        digest, archive = artifact
        root, _, _ = self.environment(label)
        transferred = []
        for index, source in enumerate([archive] + ([child_archive] if child_archive is not None else [])):
            copied = root / ("component-" + str(index) + ".oci.tar")
            shutil.copyfile(source, copied)
            self.run(label + "-load-" + str(index), self.native(label) + ["component", "load", "--input", copied], label)
            transferred.append({"path": str(copied), "sha256": frontend.file_hash(copied)})
        caller = root / "caller"
        caller.mkdir()
        definition = caller / "image.coopr"
        call = 'component ' + json.dumps(digest)
        if message is not None:
            call += ' { arg "message" "${message}"; }'
        hello = ('test -x "$CHILD_TOOL"; "$CHILD_TOOL" > /hello; '
                 if child_archive is not None else
                 'test -x /bin/hello; /bin/hello > /hello; ')
        definition.write_text('arg "message" "consumer-default"\nfrom "scratch" {\n'
                              ' arg "message"\n'
                              ' env CALLER_MARK="retained" PATH="/caller/bin:/bin"\n'
                              ' workdir "/caller"\n cmd { exec "/caller-command"; }\n '
                              + call + '\n run { exec "/bin/bash" "-euc" '
                              + json.dumps('test "$CALLER_MARK" = retained; test "$PWD" = /caller; '
                                           'test -L /bin/bash; ' + hello +
                                           'echo NIX_COMPONENT_CONSUMER_RAN') + '; }\n}\n')
        (self.evidence / (label + ".coopr")).write_text(definition.read_text())
        previous = self.context
        hidden = previous.with_name(previous.name + "-unavailable")
        previous.rename(hidden)
        self.context = caller
        try:
            results = []
            expected_input = self.inspect(Path(recipe["input"])) if recipe is not None else None
            for attempt in (["cold", "warm", "changed"] if message is not None else ["cold", "warm"]):
                invocation = message if attempt != "changed" else message + "changed invocation\n"
                key = label + "-" + attempt
                arguments = [] if message is None else ["--build-arg", "message=" + invocation]
                _, text = self.run(key, self.native(label) + ["build", "--file", definition]
                                   + self.build_options(key, label) + arguments + [caller], label)
                assert "NIX_CUSTOM_PACKAGE_RAN\n" not in text
                image_id = (self.evidence / (key + ".iid")).read_text().strip()
                view = self.export(key, image_id, label)
                config = view["config"]["config"]
                assert config["WorkingDir"] == "/caller" and config["Cmd"] == ["/caller-command"]
                assert "CALLER_MARK=retained" in config["Env"] and "PATH=/caller/bin:/bin" in config["Env"]
                assert not config.get("Entrypoint")
                assert view["files"]["hello"] == "Hello, world!\n"
                if recipe is not None:
                    assert view["config"]["architecture"] == recipe["platform"].split("/")[1]
                    for name in ["bin/bash", "bin/hello", "bin/readlink"]:
                        assert view["symlinks"][name] == expected_input["symlinks"][name]
                    assert view["files"]["proof"] == invocation
                    assert not any(value.startswith("CAPTURED_MESSAGE=") for value in config["Env"])
                if child_archive is not None:
                    assert "CHILD_CONFIG=retained" in config["Env"]
                results.append({"imageID": image_id, "image": view,
                                "childRan": "NIX_COMPONENT_CHILD_RAN\n" in text,
                                "customRun": "NIX_CUSTOM_COMPONENT_RAN\n" in text,
                                "consumerRan": "NIX_COMPONENT_CONSUMER_RAN\n" in text})
            cold, warm = results[:2]
            assert cold["imageID"] == warm["imageID"]
            assert cold["image"]["manifest"] == warm["image"]["manifest"]
            assert cold["consumerRan"] and not warm["consumerRan"]
            if child_archive is not None:
                assert cold["childRan"] and not warm["childRan"]
            if message is not None:
                assert cold["customRun"] and not warm["customRun"]
                changed = results[2]
                assert changed["customRun"] and changed["consumerRan"]
                assert changed["imageID"] != cold["imageID"]
                assert changed["image"]["files"]["input-revision"] == cold["image"]["files"]["input-revision"]
            self.results[label] = {"cold": cold, "warm": warm, "transferredArchives": transferred,
                                   "producerContextUnavailable": True, "producerNamedContextSupplied": False,
                                   **({"changed": changed} if message is not None else {})}
            return cold["image"]
        finally:
            self.context = previous
            if previous.exists():
                raise RuntimeError("private producer context path was unexpectedly replaced")
            hidden.rename(previous)

    def delivery_child(self):
        return self.component("archive-selected-child", '''extend {
  arg "message" "default"
  arg "tool"
  env CHILD_CONFIG="retained" CHILD_MESSAGE="${message}" CHILD_TOOL="${tool}"
  run {
    exec "/bin/bash" "-euc" "\\\"$CHILD_TOOL\\\" > /hello; printf '%s' \\\"$CHILD_MESSAGE\\\" > /proof; echo NIX_COMPONENT_CHILD_RAN"
  }
}
''')

    def complete_delivery(self):
        child = self.delivery_child()
        literal = 'literal \' and " and ${CALLER_MARK} and $(not-a-command)\\tail\nsecond line\n'
        program, recipe = self.declaration_component("nix-archive-delivery", child[0], literal, archive=child[1])
        bundle = self.evidence / "complete-parent.bale"
        artifact = self.produce("complete-parent", program, bundle)
        self.results["completeDelivery"] = self.consume_complete_delivery("complete-delivery", artifact, bundle, literal, recipe)

    def consume_complete_delivery(self, label, artifact, bundle, literal, recipe, selected_context=""):
        receipt = self.evidence / (label + "-receipt.json")
        archive = self.evidence / (label + ".bale.tar")
        root, env, _ = self.environment(label)
        env.update(COOPR_NIX_BUNDLE=str(bundle), COOPR_NIX_PARENT_DIGEST=artifact[0],
                   COOPR_NIX_EXPECTED_LITERAL=literal, COOPR_NIX_RUNTIME=self.options.runtime,
                   COOPR_NIX_BUNDLE_RECEIPT=str(receipt), COOPR_NIX_BUNDLE_ARCHIVE=str(archive),
                   COOPR_NIX_CONSUMER_SCRATCH=str(root / "tmp"),
                   COOPR_NIX_EXPECTED_DELIVERY_CONTEXT=selected_context,
                   COOPR_NIX_EXPECTED_CHILD_REFERENCE=recipe["components"][0]["invocationReference"])
        previous = self.context
        hidden = previous.with_name(previous.name + "-unavailable")
        previous.rename(hidden)
        caller = root / "empty-caller"
        caller.mkdir()
        self.context = caller
        try:
            self.run(label, ["go", "-C", self.repo, "test", "-v", "-count=1", "-p=1", "-timeout=5m",
                     "-run", "^TestConsumeNixCompleteBundle$", "./internal/build"], label)
        finally:
            self.context = previous
            if previous.exists():
                raise RuntimeError("private producer source unexpectedly reappeared")
            hidden.rename(previous)
        proof = json.loads(receipt.read_text())
        assert proof["status"] == "PASS" and proof["parent"] == artifact[0]
        proof.update(completeArchiveSHA256=frontend.file_hash(archive),
             producerContextUnavailableDuringConsumption=True, archiveSelectedRecipe=recipe,
             completeSingleArchiveBundleQualified=True, archiveSpecificBaleDeliveryQualified=True)
        return proof

    def bound_complete_delivery(self):
        child = self.delivery_child()
        fixtures = self.evidence / "selected-child-fixtures"
        _, env, _ = self.environment("prepare-bound-fixtures")
        env.update(COOPR_NIX_DELIVERY_FIXTURE_INPUT=str(child[1]),
                   COOPR_NIX_DELIVERY_FIXTURE_OUTPUT=str(fixtures))
        self.run("prepare-bound-fixtures", ["go", "-C", self.repo, "test", "-v", "-count=1", "-p=1", "-timeout=1m",
                 "-run", "^TestPrepareNixDeliveryArchives$", "./internal/oci"], "prepare-bound-fixtures")
        selections = json.loads((fixtures / "fixtures.json").read_text())
        assert len(selections) == 2 and all(value["digest"] == child[0] for value in selections)
        assert frontend.file_hash(Path(selections[0]["archive"])) != frontend.file_hash(Path(selections[1]["archive"]))
        produced, bindings = [], []
        for selection in selections:
            name = "bound-" + selection["context"]
            literal = name + ' literal ${CALLER_MARK} $(not-a-command)\nlast line\n'
            program, recipe = self.declaration_component("nix-" + name, child[0], literal,
                archive=selection["archive"], bundle=selection["bundle"])
            bundle = self.evidence / (name + ".bale")
            artifact = self.produce(name, program, bundle)
            self.results[name] = self.consume_complete_delivery(name + "-consumer", artifact, bundle, literal, recipe, selection["context"])
            produced.append((program, artifact, recipe))
        for selection, (_, _, recipe) in zip(selections, produced):
            reference = recipe["components"][0]["invocationReference"]
            _, text = self.run("inspect-bound-" + selection["context"], self.native() + ["component", "inspect", reference])
            descriptor = json.loads(text)
            binding = json.loads(descriptor["annotations"]["dev.coopr.strio.component.binding"])
            assert descriptor["digest"] == child[0] and binding["repository"] == selection["repository"]
            bindings.append({"reference": reference, "binding": binding})
        assert bindings[0]["reference"] != bindings[1]["reference"] and bindings[0]["binding"] != bindings[1]["binding"]
        status, text = self.run("inspect-bare-bound-child", self.native() + ["component", "inspect", child[0]], expect=None)
        assert status != 0 and "multiple delivery contexts" in text
        repeated = self.evidence / "bound-a-after-b.bale"
        again = self.produce("bound-a-after-b", produced[0][0], repeated)
        original_metadata = json.loads((self.evidence / "bound-a.metadata.json").read_text())
        repeated_metadata = json.loads((self.evidence / "bound-a-after-b.metadata.json").read_text())
        assert again[0] == produced[0][1][0]
        assert original_metadata["coopr.bundle"]["descriptor"] == repeated_metadata["coopr.bundle"]["descriptor"]
        self.results["boundArchiveDelivery"] = {"sameChildRoot": child[0], "bindings": bindings,
            "selectedContexts": [value["context"] for value in selections], "bareDigestAmbiguous": True,
            "aAfterBProducesSameRootAndBundle": True, "sourceRegistriesStoppedBeforeNix": True,
            "completeSingleArchiveBundleQualified": True, "sameRootBoundABQualified": True}

    def execute(self):
        if self.options.bound_delivery_only:
            self.bound_complete_delivery()
            self.results.update(status="PASS", sameHostAndUID=True, crossHostQualified=False,
                                scratchBytes=self.check_size(), runningOwnedOperations=[])
            frontend.json_write(self.evidence / "result.json", self.results)
            return self.results
        if self.options.delivery_only:
            self.complete_delivery()
            self.results.update(status="PASS", sameHostAndUID=True, crossHostQualified=False,
                                scratchBytes=self.check_size(), runningOwnedOperations=[])
            frontend.json_write(self.evidence / "result.json", self.results)
            return self.results
        self.empty_image("empty-image")
        empty_child = self.component("empty-child", 'extend { env EMPTY_CHILD="retained"; workdir "/child-config"; }')
        self.empty_image("components-only-image", empty_child[0])
        plain, _ = self.declaration_component("nix-component-plain")
        artifact = self.produce("plain-package", plain)
        self.consume("plain-consumer", artifact)

        child = self.component("nix-child", '''extend {
  arg "message" "default"
  arg "tool"
  env CHILD_CONFIG="retained" CHILD_MESSAGE="${message}" CHILD_TOOL="${tool}"
  run {
    exec "/bin/bash" "-euc" "\\\"$CHILD_TOOL\\\" > /hello; printf '%s' \\\"$CHILD_MESSAGE\\\" > /proof; echo NIX_COMPONENT_CHILD_RAN"
  }
}
''')
        literal = 'quote \' and " and ${CALLER_MARK} and $(not-a-command)\\tail\nsecond line\n'
        nested, _ = self.declaration_component("nix-component-nested", child[0], literal)
        nested_artifact = self.produce("nested-package", nested)
        first = self.consume("nested-consumer", nested_artifact, child[1])
        assert first["files"]["proof"] == literal
        changed, _ = self.declaration_component("nix-component-changed", child[0], "changed literal")
        changed_artifact = self.produce("changed-package", changed)
        assert changed_artifact[0] != nested_artifact[0]
        second = self.consume("changed-consumer", changed_artifact, child[1])
        assert second["files"]["proof"] == "changed literal"

        custom_definition = self.context / "custom-component.coopr"
        custom_definition.write_text('''arg "published"
from "coopr-nix" as="input" {
  arg "published"
  env CAPTURED_MESSAGE="${published}"
  run {
    exec "/bin/bash" "-euc" "printf '%s' \\\"$CAPTURED_MESSAGE\\\" > /input-revision; echo NIX_CUSTOM_PACKAGE_RAN"
  }
}
package as="closure" { copy "/" "/" from="input"; }
extend {
  arg "message" "default"
  copy "/" "/" from="closure"
  env COMPONENT_MESSAGE="${message}"
  run {
    exec "/bin/bash" "-euc" "printf '%s' \\\"$COMPONENT_MESSAGE\\\" > /proof; echo NIX_CUSTOM_COMPONENT_RAN"
  }
}
''')
        custom, custom_recipe = self.declaration_component("nix-component-custom", definition=custom_definition,
                                                args={"published": literal})
        custom_artifact = self.produce("custom-package", custom)
        assert "NIX_CUSTOM_PACKAGE_RAN\n" in (self.evidence / "custom-package.log").read_text()
        invocation = 'consumer $value "quotes" $(not-a-command)\nlast line\n'
        custom_image = self.consume("custom-consumer", custom_artifact, message=invocation, recipe=custom_recipe)
        assert custom_image["files"]["input-revision"] == literal
        assert custom_image["files"]["proof"] == invocation
        assert "CAPTURED_MESSAGE=" + literal not in custom_image["config"]["config"]["Env"]
        self.complete_delivery()
        self.results.update({"status": "PASS", "nativeComponentProduction": True,
                             "emptyAndComponentsOnlyImageConstructors": True,
                             "nativeChildRunDeferredUntilConsumption": True,
                             "literalArgumentsPreserved": True, "argumentOnlyClosureExecuted": True,
                             "callerConfigurationPreserved": True, "coldWarmAndArgumentInvalidation": True,
                             "customNativePackageAndInvocationRunPreserved": True,
                             "consumerArchivesExplicitlyProvided": True,
                             "completeSingleArchiveBundleQualified": True,
                             "archiveSpecificBaleDeliveryQualified": True,
                             "sameRootBoundABQualified": False,
                             "sameHostAndUID": True, "crossHostQualified": False,
                             "scratchBytes": self.check_size(), "runningOwnedOperations": [],
                             "cooprExecutedInsideNixSandbox": False, "hostNixExecutor": False})
        frontend.json_write(self.evidence / "result.json", self.results)
        return self.results


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--repo", type=Path, default=Path(__file__).resolve().parents[2])
    parser.add_argument("--coopr-binary", type=Path, required=True)
    parser.add_argument("--evidence", type=Path, required=True)
    parser.add_argument("--scratch-parent", type=Path, required=True)
    parser.add_argument("--scratch-limit-gib", type=float, default=6)
    parser.add_argument("--system", choices=["x86_64-linux", "aarch64-linux"],
                        default={"x86_64": "x86_64-linux", "aarch64": "aarch64-linux"}.get(os.uname().machine))
    parser.add_argument("--nix-binary", default="nix")
    parser.add_argument("--runtime", default="crun", help="runtime executable path or name on PATH")
    parser.add_argument("--helper-directory", action="append")
    parser.add_argument("--delivery-only", action="store_true", help="run only the new complete archive delivery path")
    parser.add_argument("--bound-delivery-only", action="store_true", help="qualify complete delivery of same-root A/B archive selections")
    parser.add_argument("--timeout", type=int, default=600)
    options = parser.parse_args()
    runtime = shutil.which(options.runtime)
    if runtime is None:
        parser.error(f"runtime executable not found or not executable: {options.runtime}")
    options.runtime = os.path.abspath(runtime)
    if options.delivery_only and options.bound_delivery_only:
        parser.error("select only one focused delivery mode")
    if options.system is None:
        parser.error("a supported native --system is required")
    native_system = {"x86_64": "x86_64-linux", "aarch64": "aarch64-linux"}.get(os.uname().machine)
    if options.system != native_system:
        parser.error("this qualification requires the matching native system; emulation is a separate check")
    qualification = Qualification(options)

    def interrupted(number, _frame):
        # Unwind subprocess.wait before the exception handler drains the child.
        raise InterruptedError(f"acceptance interrupted by signal {number}")

    signal.signal(signal.SIGTERM, interrupted)
    signal.signal(signal.SIGHUP, interrupted)
    try:
        result = qualification.execute()
        qualification.cleanup_success()
    except BaseException as error:
        qualification.stop_active()
        qualification.results["status"] = "FAILED"
        frontend.json_write(qualification.evidence / "result.json", qualification.results)
        frontend.json_write(qualification.evidence / "failure.json", {
            "status": "FAILED", "error": repr(error), "scratch": str(qualification.scratch),
            "operations": qualification.operations, "results": qualification.results})
        raise
    print(json.dumps(result, indent=2))


if __name__ == "__main__":
    main()
