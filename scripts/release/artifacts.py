"""Check required files in release archives before they are uploaded.

Usage: python3 scripts/release/artifacts.py VERSION OUT_DIR
"""

import argparse
import json
import sys
import tarfile
import zipfile
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
TARGETS = ("darwin_arm64", "linux_amd64", "linux_arm64", "windows_amd64")


def tar_file(archive: tarfile.TarFile, name: str) -> bytes:
    try:
        member = archive.getmember(name)
    except KeyError:
        raise ValueError(f"{archive.name}: missing {name}") from None
    if not member.isfile():
        raise ValueError(f"{archive.name}: {name} is not a regular file")
    with archive.extractfile(member) as source:
        return source.read()


def zip_file(archive: zipfile.ZipFile, name: str) -> bytes:
    try:
        member = archive.getinfo(name)
    except KeyError:
        raise ValueError(f"{archive.filename}: missing {name}") from None
    if member.is_dir():
        raise ValueError(f"{archive.filename}: {name} is not a file")
    return archive.read(member)


def check_contents(data: bytes, expected: bytes, location: str) -> None:
    if data != expected:
        raise ValueError(f"{location}: contents do not match the source file")


def check_artifacts(root: Path, out: Path, version: str) -> None:
    notices = {
        "LICENSE": (root / "LICENSE").read_bytes(),
        "NOTICE": (root / "NOTICE").read_bytes(),
        "GO-LICENSE": (root / "third_party/go/LICENSE").read_bytes(),
    }
    for target in TARGETS:
        name = f"stave_{version}_{target}"
        if target == "windows_amd64":
            file = out / f"{name}.zip"
            with zipfile.ZipFile(file) as archive:
                if not zip_file(archive, f"{name}/stave.exe"):
                    raise ValueError(f"{file}: stave.exe is empty")
                for notice, expected in notices.items():
                    member = f"{name}/{notice}"
                    check_contents(
                        zip_file(archive, member), expected, f"{file}: {member}"
                    )
        else:
            file = out / f"{name}.tar.gz"
            with tarfile.open(file, "r:gz") as archive:
                if not tar_file(archive, f"{name}/stave"):
                    raise ValueError(f"{file}: stave is empty")
                for notice, expected in notices.items():
                    member = f"{name}/{notice}"
                    check_contents(
                        tar_file(archive, member), expected, f"{file}: {member}"
                    )

    extension = root / "editors/vscode"
    extension_version = json.loads((extension / "package.json").read_text())["version"]
    file = out / f"stave-vscode-{extension_version}.vsix"
    helper = "extension/out/terminateProcess.sh"
    required = {
        # vsce gives the source LICENSE a .txt suffix in the VSIX.
        "extension/LICENSE.txt": notices["LICENSE"],
        "extension/THIRD_PARTY_NOTICES.txt": (
            extension / "THIRD_PARTY_NOTICES.txt"
        ).read_bytes(),
        helper: (
            extension
            / "node_modules/vscode-languageclient/lib/node/terminateProcess.sh"
        ).read_bytes(),
    }
    with zipfile.ZipFile(file) as archive:
        if not zip_file(archive, "extension/out/extension.js"):
            raise ValueError(f"{file}: extension/out/extension.js is empty")
        for member, expected in required.items():
            check_contents(zip_file(archive, member), expected, f"{file}: {member}")
        if not (archive.getinfo(helper).external_attr >> 16) & 0o111:
            raise ValueError(f"{file}: {helper} is not executable")


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("version")
    parser.add_argument("out", type=Path)
    args = parser.parse_args()
    try:
        check_artifacts(ROOT, args.out, args.version)
    except (OSError, ValueError, tarfile.TarError, zipfile.BadZipFile) as exc:
        print(f"release artifacts: {exc}", file=sys.stderr)
        return 1
    print("release artifacts: binary notices and VSIX notices/process helper verified")
    return 0


if __name__ == "__main__":
    sys.exit(main())
