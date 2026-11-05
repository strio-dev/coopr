{ pkgs, buildImage }:
let
  message = pkgs.writeText "coopr-image-message" "closure input\n";
  root = pkgs.runCommand "coopr-image-fixture-root" { } ''
    mkdir -p "$out/etc"
    ln -s ${message} "$out/etc/message"
  '';
  imageArgs = {
    name = "coopr-nix-check";
    tag = "test";
    packages = [ pkgs.hello ];
    copyToRoot = [ root ];
    config = {
      Entrypoint = [ "${pkgs.hello}/bin/hello" ];
      Env = [
        "PATH=/bin"
        "COOPR_NIX_CHECK=1"
      ];
      WorkingDir = "/";
      Labels."dev.strio.coopr.nix-check" = "closure";
    };
  };
  image = buildImage imageArgs;
  changedMessage = pkgs.writeText "coopr-image-message" "changed closure input\n";
  changedRoot = pkgs.runCommand "coopr-image-fixture-root" { } ''
    mkdir -p "$out/etc"
    ln -s ${changedMessage} "$out/etc/message"
  '';
  contentImage = buildImage (imageArgs // { copyToRoot = [ changedRoot ]; });
  configImage = buildImage (
    imageArgs
    // {
      config = imageArgs.config // {
        Labels = imageArgs.config.Labels // {
          "dev.strio.coopr.nix-check" = "changed";
        };
      };
    }
  );
  closure = pkgs.closureInfo {
    rootPaths = [
      pkgs.hello
      message
    ];
  };
  expectedPlatform = if pkgs.stdenv.hostPlatform.isx86_64 then "amd64" else "arm64";
in
pkgs.runCommand "coopr-nix-image-check"
  {
    nativeBuildInputs = [ pkgs.python3 ];
    passthru = { inherit image; };
    inherit (image.inputImage) oci;
    inherit closure;
    helloPath = pkgs.hello;
    changedMessagePath = changedMessage;
    contentOci = contentImage.inputImage.oci;
    configOci = configImage.inputImage.oci;
    architecture = expectedPlatform;
  }
  ''
    mkdir "$out"
    python3 - <<'PY'
    import hashlib
    import io
    import json
    import os
    import posixpath
    import stat
    import tarfile
    from pathlib import Path

    layout = Path(os.environ["oci"])
    assert json.loads((layout / "oci-layout").read_bytes())["imageLayoutVersion"] == "1.0.0"
    index = json.loads((layout / "index.json").read_bytes())
    assert len(index["manifests"]) == 1
    root_descriptor = index["manifests"][0]
    assert root_descriptor["annotations"]["org.opencontainers.image.ref.name"] == "image"

    def blob(descriptor, source=layout):
        algorithm, digest = descriptor["digest"].split(":", 1)
        data = (source / "blobs" / algorithm / digest).read_bytes()
        assert len(data) == descriptor["size"]
        assert hashlib.new(algorithm, data).hexdigest() == digest
        return data

    manifest = json.loads(blob(root_descriptor))
    config = json.loads(blob(manifest["config"]))
    assert config["os"] == "linux"
    assert config["architecture"] == os.environ["architecture"]
    assert config["config"]["Entrypoint"] == [os.environ["helloPath"] + "/bin/hello"]
    assert config["config"]["Env"] == ["PATH=/bin", "COOPR_NIX_CHECK=1"]
    assert config["config"]["WorkingDir"] == "/"
    assert config["config"]["Labels"]["dev.strio.coopr.nix-check"] == "closure"
    assert config["created"] == "0001-01-01T00:00:00Z"

    def read_members(manifest, config, source):
        members = {}
        for descriptor, diff_id in zip(manifest["layers"], config["rootfs"]["diff_ids"], strict=True):
            data = blob(descriptor, source)
            # Check the uncompressed tar digest separately from its OCI blob.
            if descriptor["mediaType"].endswith("+gzip"):
                import gzip
                uncompressed = gzip.decompress(data)
            else:
                uncompressed = data
            assert "sha256:" + hashlib.sha256(uncompressed).hexdigest() == diff_id
            with tarfile.open(fileobj=io.BytesIO(data), mode="r:*") as archive:
                for entry in archive:
                    path = "/" + entry.name.removeprefix("./").lstrip("/")
                    assert posixpath.normpath(path) == path
                    assert entry.mtime == 1, (path, entry.mtime)
                    assert entry.uid == 0 and entry.gid == 0, path
                    members[path] = (entry, archive.extractfile(entry).read() if entry.isfile() else None)

        return members

    members = read_members(manifest, config, layout)

    # Resolve links against the image view, never the builder's original store.
    def resolve(path, view=members, depth=0):
        assert depth < 64, path
        parts = path.strip("/").split("/")
        for i in range(1, len(parts) + 1):
            prefix = "/" + "/".join(parts[:i])
            if prefix in view and view[prefix][0].issym():
                target = view[prefix][0].linkname
                if not target.startswith("/"):
                    target = posixpath.join(posixpath.dirname(prefix), target)
                return resolve(posixpath.normpath(posixpath.join(target, *parts[i:])), view, depth + 1)
        assert path in view, path
        return view[path]

    assert resolve("/bin/hello")[0].isfile()
    assert resolve("/bin/hello")[1] == Path(os.environ["helloPath"] + "/bin/hello").read_bytes()
    assert resolve("/etc/message")[1] == b"closure input\n"
    closure_entries = 0
    for store_path in (Path(os.environ["closure"]) / "store-paths").read_text().splitlines():
        expected = [store_path]
        if Path(store_path).is_dir():
            for directory, subdirs, files in os.walk(store_path, followlinks=False):
                expected.extend(os.path.join(directory, name) for name in subdirs + files)
        for path in expected:
            assert path in members, path
            entry, data = members[path]
            source = os.lstat(path)
            if stat.S_ISLNK(source.st_mode):
                assert entry.issym() and entry.linkname == os.readlink(path), path
            elif stat.S_ISDIR(source.st_mode):
                assert entry.isdir(), path
            else:
                assert stat.S_ISREG(source.st_mode) and entry.isfile(), path
                assert data == Path(path).read_bytes(), path
            assert entry.mode & 0o111 == source.st_mode & 0o111, path
            closure_entries += 1
    variants = {}
    for name, variable in [("content", "contentOci"), ("config", "configOci")]:
        source = Path(os.environ[variable])
        variant_index = json.loads((source / "index.json").read_bytes())
        assert len(variant_index["manifests"]) == 1
        descriptor = variant_index["manifests"][0]
        variant_manifest = json.loads(blob(descriptor, source))
        variant_config = json.loads(blob(variant_manifest["config"], source))
        assert descriptor["digest"] != root_descriptor["digest"], name
        assert variant_manifest["config"]["digest"] != manifest["config"]["digest"], name
        assert variant_config["architecture"] == config["architecture"]
        assert variant_config["os"] == config["os"]
        variant_members = read_members(variant_manifest, variant_config, source)
        layers = [layer["digest"] for layer in variant_manifest["layers"]]
        original_layers = [layer["digest"] for layer in manifest["layers"]]
        if name == "content":
            assert layers != original_layers
            assert variant_config["rootfs"]["diff_ids"] != config["rootfs"]["diff_ids"]
            assert resolve("/etc/message", variant_members)[1] == b"changed closure input\n"
            assert variant_members["/etc/message"][0].linkname == os.environ["changedMessagePath"]
            assert variant_config["config"] == config["config"]
        else:
            assert layers == original_layers
            assert variant_config["rootfs"] == config["rootfs"]
            assert variant_config["config"]["Labels"]["dev.strio.coopr.nix-check"] == "changed"
            expected_config = dict(config["config"])
            expected_config["Labels"] = dict(expected_config["Labels"])
            expected_config["Labels"]["dev.strio.coopr.nix-check"] = "changed"
            assert variant_config["config"] == expected_config
            assert resolve("/etc/message", variant_members)[1] == b"closure input\n"
        variants[name] = {
            "manifest": descriptor["digest"],
            "ociLayout": str(source),
            "config": variant_manifest["config"]["digest"],
            "layerDigests": layers,
            "layersChanged": layers != original_layers,
            "standaloneLayoutBytes": sum(path.stat().st_size for path in source.rglob("*") if path.is_file()),
        }
    result = {
        "manifest": root_descriptor["digest"],
        "ociLayout": str(layout),
        "invalidation": variants,
        "files": len(members),
        "closureEntriesVerified": closure_entries,
        "platform": config["os"] + "/" + config["architecture"],
        "standaloneLayoutBytes": sum(path.stat().st_size for path in layout.rglob("*") if path.is_file()),
        "blobBytes": sum(path.stat().st_size for path in (layout / "blobs").rglob("*") if path.is_file()),
        "closureStorePaths": (Path(os.environ["closure"]) / "store-paths").read_text().splitlines(),
    }
    (Path(os.environ["out"]) / "result.json").write_text(json.dumps(result, indent=2) + "\n")
    inventory = [{"path": path, "type": "symlink" if member.issym() else "directory" if member.isdir() else "file", "link": member.linkname, "bytes": member.size} for path, (member, _) in sorted(members.items())]
    (Path(os.environ["out"]) / "files.json").write_text(json.dumps(inventory, indent=2) + "\n")
    print(json.dumps(result))
    PY
  ''
