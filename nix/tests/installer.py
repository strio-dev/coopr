"""Exercise the real installer without network access or user-directory writes."""

import hashlib
import io
import os
from pathlib import Path
import subprocess
import sys
import tarfile
import tempfile
import unittest

INSTALLER = Path(sys.argv.pop(1)).resolve()
RELEASES = "https://github.com/strio-dev/coopr/releases"

MOCK = '''#!{python}
import os, pathlib, shutil, sys
if pathlib.Path(sys.argv[0]).name == "uname":
    print(os.environ["TEST_OS"] if sys.argv[1] == "-s" else os.environ["TEST_ARCH"])
else:
    url = sys.argv[-1]
    with open(os.environ["TEST_REQUESTS"], "a") as log:
        log.write(url + "\\n")
    if "-w" in sys.argv:
        print(os.environ["TEST_LATEST"], end="")
    else:
        if "/download/1.2.3/" not in url:
            sys.exit(22)
        source = pathlib.Path(os.environ["TEST_ASSETS"]) / url.rsplit("/", 1)[1]
        shutil.copyfile(source, sys.argv[sys.argv.index("-o") + 1])
'''


class InstallerTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        for name in ("tools", "assets", "home", "tmp"):
            (self.root / name).mkdir()
        for name in ("curl", "uname"):
            tool = self.root / "tools" / name
            tool.write_text(MOCK.format(python=sys.executable))
            tool.chmod(0o755)
        checksums = []
        for arch in ("amd64", "arm64"):
            asset = self.root / "assets" / f"coopr-linux-{arch}.tar.gz"
            with tarfile.open(asset, "w:gz") as archive:
                for name, contents, mode in [
                    ("coopr", b"#!/bin/sh\nprintf 'coopr fixture\\n'\n", 0o555),
                    ("LICENSE", b"MIT license fixture\n", 0o444),
                    ("third-party/NOTICE", b"Dependency notices fixture\n", 0o444),
                ]:
                    info = tarfile.TarInfo(name)
                    info.size, info.mode = len(contents), mode
                    archive.addfile(info, io.BytesIO(contents))
            checksums.append(f"{hashlib.sha256(asset.read_bytes()).hexdigest()}  ./{asset.name}\n")
        (self.root / "assets" / "SHA256SUMS").write_text("".join(checksums))
        self.env = {
            **os.environ,
            "PATH": f"{self.root / 'tools'}:{os.environ['PATH']}",
            "HOME": str(self.root / "home"),
            "TMPDIR": str(self.root / "tmp"),
            "TEST_OS": "Linux",
            "TEST_ARCH": "x86_64",
            "TEST_LATEST": f"{RELEASES}/tag/1.2.3",
            "TEST_ASSETS": str(self.root / "assets"),
            "TEST_REQUESTS": str(self.root / "requests"),
        }
        self.env.pop("COOPR_INSTALL_PREFIX", None)

    def run_installer(self, *args, success=True):
        result = subprocess.run(["sh", str(INSTALLER), *args], env=self.env, capture_output=True, text=True)
        self.assertEqual(result.returncode == 0, success, result.stdout + result.stderr)
        self.assertEqual(list((self.root / "tmp").iterdir()), [], "temporary files leaked")
        return result

    def test_latest_install_and_repeat(self):
        self.run_installer()
        prefix = self.root / "home" / ".local"
        self.assertEqual(subprocess.check_output([prefix / "bin/coopr"], text=True), "coopr fixture\n")
        self.assertEqual((prefix / "share/licenses/coopr/LICENSE").read_text(), "MIT license fixture\n")
        self.assertEqual((prefix / "share/licenses/coopr/third-party/NOTICE").read_text(), "Dependency notices fixture\n")
        self.assertEqual((self.root / "requests").read_text().splitlines(), [
            f"{RELEASES}/latest", f"{RELEASES}/download/1.2.3/coopr-linux-amd64.tar.gz",
            f"{RELEASES}/download/1.2.3/SHA256SUMS",
        ])
        self.run_installer()

    def test_exact_version_arm64_custom_prefix(self):
        self.env.update(TEST_ARCH="aarch64", COOPR_INSTALL_PREFIX=str(self.root / "custom prefix"))
        self.run_installer("1.2.3")
        self.assertTrue((self.root / "custom prefix/bin/coopr").is_file())
        self.assertIn("coopr-linux-arm64.tar.gz", (self.root / "requests").read_text())
        self.assertNotIn("/latest", (self.root / "requests").read_text())

    def test_bad_checksum_preserves_existing_install(self):
        self.run_installer()
        binary = self.root / "home/.local/bin/coopr"
        before = binary.read_bytes()
        (self.root / "assets/coopr-linux-amd64.tar.gz").write_bytes(b"corrupted download")
        self.run_installer(success=False)
        self.assertEqual(binary.read_bytes(), before)

    def test_missing_checksum(self):
        (self.root / "assets/SHA256SUMS").write_text("")
        self.assertIn("Missing checksum", self.run_installer(success=False).stderr)
        self.assertFalse((self.root / "home/.local").exists())

    def test_no_release_and_unsupported_platform(self):
        self.env["TEST_LATEST"] = RELEASES
        self.assertIn("No published release", self.run_installer(success=False).stderr)
        self.env["TEST_OS"] = "Darwin"
        self.assertIn("requires Linux", self.run_installer(success=False).stderr)
        self.env.update(TEST_OS="Linux", TEST_ARCH="riscv64")
        self.assertIn("Supported architectures", self.run_installer(success=False).stderr)


if __name__ == "__main__":
    unittest.main()
