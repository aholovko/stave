"""Small archive fixtures keep packaging checks independent of build tools and npm."""

import io
import json
import stat
import tarfile
import tempfile
import unittest
import zipfile
from pathlib import Path

from artifacts import TARGETS, check_artifacts

VERSION = "v0.1.0"
HELPER = "extension/out/terminateProcess.sh"


class ArtifactTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.out = self.root / "release"
        self.out.mkdir()
        self.notices = {
            "LICENSE": b"STAVE license\n",
            "NOTICE": b"STAVE notice\n",
            "GO-LICENSE": b"Go license\n",
        }
        self.vsix = {
            "extension/LICENSE.txt": self.notices["LICENSE"],
            "extension/THIRD_PARTY_NOTICES.txt": b"Bundled dependency licenses\n",
            "extension/out/extension.js": b"// bundled extension\n",
            HELPER: b'#!/bin/sh\nkill -TERM "$1"\n',
        }
        sources = {
            "LICENSE": self.notices["LICENSE"],
            "NOTICE": self.notices["NOTICE"],
            "third_party/go/LICENSE": self.notices["GO-LICENSE"],
            "editors/vscode/package.json": json.dumps({"version": "0.1.0"}).encode(),
            "editors/vscode/THIRD_PARTY_NOTICES.txt": self.vsix[
                "extension/THIRD_PARTY_NOTICES.txt"
            ],
            "editors/vscode/node_modules/vscode-languageclient/lib/node/terminateProcess.sh": self.vsix[
                HELPER
            ],
        }
        for name, data in sources.items():
            file = self.root / name
            file.parent.mkdir(parents=True, exist_ok=True)
            file.write_bytes(data)
        for target in TARGETS:
            self.write_binary(target)
        self.write_vsix()

    def write_binary(self, target, missing="", changed=""):
        name = f"stave_{VERSION}_{target}"
        binary = "stave.exe" if target == "windows_amd64" else "stave"
        files = {**self.notices, binary: b"binary fixture"}
        files.pop(missing, None)
        if changed:
            files[changed] = b"incorrect contents"
        if target == "windows_amd64":
            with zipfile.ZipFile(self.out / f"{name}.zip", "w") as archive:
                for member, data in files.items():
                    archive.writestr(f"{name}/{member}", data)
        else:
            with tarfile.open(self.out / f"{name}.tar.gz", "w:gz") as archive:
                for member, data in files.items():
                    info = tarfile.TarInfo(f"{name}/{member}")
                    info.size = len(data)
                    info.mode = 0o755 if member == binary else 0o644
                    archive.addfile(info, io.BytesIO(data))

    def write_vsix(self, missing="", changed="", helper_mode=0o755):
        with zipfile.ZipFile(self.out / "stave-vscode-0.1.0.vsix", "w") as archive:
            for member, data in self.vsix.items():
                if member == missing:
                    continue
                info = zipfile.ZipInfo(member)
                # Set ZIP metadata explicitly so these fixtures also work on Windows.
                info.create_system = 3
                mode = helper_mode if member == HELPER else 0o644
                info.external_attr = (stat.S_IFREG | mode) << 16
                archive.writestr(
                    info, b"incorrect contents" if member == changed else data
                )

    def check(self):
        check_artifacts(self.root, self.out, VERSION)

    def test_complete_archives_pass(self):
        self.check()

    def test_every_binary_archive_requires_its_notices_and_binary(self):
        for target in TARGETS:
            binary = "stave.exe" if target == "windows_amd64" else "stave"
            for member in (*self.notices, binary):
                with self.subTest(target=target, member=member):
                    self.write_binary(target, missing=member)
                    with self.assertRaisesRegex(ValueError, f"missing .*/{member}"):
                        self.check()
                    self.write_binary(target)

    def test_binary_notices_match_their_sources(self):
        for target in ("linux_amd64", "windows_amd64"):
            with self.subTest(target=target):
                self.write_binary(target, changed="GO-LICENSE")
                with self.assertRaisesRegex(
                    ValueError, "GO-LICENSE: contents do not match"
                ):
                    self.check()
                self.write_binary(target)

    def test_vsix_requires_notices_bundle_and_helper(self):
        for member in self.vsix:
            with self.subTest(member=member):
                self.write_vsix(missing=member)
                with self.assertRaisesRegex(ValueError, f"missing {member}"):
                    self.check()

    def test_vsix_notices_and_helper_match_their_sources(self):
        for member in (
            "extension/LICENSE.txt",
            "extension/THIRD_PARTY_NOTICES.txt",
            HELPER,
        ):
            with self.subTest(member=member):
                self.write_vsix(changed=member)
                with self.assertRaisesRegex(ValueError, "contents do not match"):
                    self.check()

    def test_vsix_helper_must_be_executable(self):
        for mode in (0o644, 0):
            with self.subTest(mode=mode):
                self.write_vsix(helper_mode=mode)
                with self.assertRaisesRegex(
                    ValueError, "terminateProcess.sh is not executable"
                ):
                    self.check()

    def test_missing_binary_archive_fails(self):
        (self.out / f"stave_{VERSION}_windows_amd64.zip").unlink()
        with self.assertRaises(FileNotFoundError):
            self.check()


if __name__ == "__main__":
    unittest.main()
