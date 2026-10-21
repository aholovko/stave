"""Validate release versions; print a version output for GitHub Actions.

Use --check to validate repository metadata without a GitHub ref. Otherwise,
GITHUB_REF_TYPE, GITHUB_REF_NAME and GITHUB_SHA select a release or development build.
Requires Python 3.11 or newer.
"""

import argparse
import json
import os
import re
import sys
import tomllib
from pathlib import Path

STABLE_VERSION = re.compile(r"(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)")
ROOT = Path(__file__).resolve().parents[2]


def component_version(root: Path) -> str:
    """Return the shared version after checking both manifests and lock files."""
    extension = json.loads(
        (root / "editors/vscode/package.json").read_text(encoding="utf-8")
    )
    npm_lock = json.loads(
        (root / "editors/vscode/package-lock.json").read_text(encoding="utf-8")
    )
    project = tomllib.loads((root / "infer/pyproject.toml").read_text(encoding="utf-8"))
    uv_lock = tomllib.loads((root / "infer/uv.lock").read_text(encoding="utf-8"))
    packages = [p for p in uv_lock.get("package", []) if p.get("name") == "stave-infer"]
    if len(packages) != 1:
        raise ValueError("infer/uv.lock must contain exactly one stave-infer package")

    versions = {
        "editors/vscode/package.json": extension["version"],
        "editors/vscode/package-lock.json": npm_lock["version"],
        "editors/vscode/package-lock.json packages['']": npm_lock["packages"][""][
            "version"
        ],
        "infer/pyproject.toml": project["project"]["version"],
        "infer/uv.lock stave-infer": packages[0]["version"],
    }
    expected = extension["version"]
    for location, value in versions.items():
        if not isinstance(value, str) or not STABLE_VERSION.fullmatch(value):
            raise ValueError(
                f"{location}: expected stable X.Y.Z version, found {value!r}"
            )
        if value != expected:
            raise ValueError(
                f"{location}: version {value} does not match "
                f"editors/vscode/package.json version {expected}"
            )
    return expected


def build_version(root: Path, ref_type: str, ref_name: str, sha: str) -> str:
    """Release tags must match package metadata; branch builds use dev-<commit>."""
    expected = component_version(root)
    if ref_type == "tag":
        if not ref_name.startswith("v") or not STABLE_VERSION.fullmatch(ref_name[1:]):
            raise ValueError(
                f"expected a stable vX.Y.Z release tag, found {ref_name!r}"
            )
        if ref_name[1:] != expected:
            raise ValueError(
                f"tag {ref_name} does not match component version {expected}"
            )
        return ref_name
    if ref_type != "branch":
        raise ValueError(
            "GITHUB_REF_TYPE must be tag or branch; use --check for local validation"
        )
    if not re.fullmatch(r"[0-9a-fA-F]{7,}", sha):
        raise ValueError(
            "GITHUB_SHA must be a hexadecimal commit hash of at least seven characters"
        )
    return f"dev-{sha[:7].lower()}"


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--check",
        action="store_true",
        help="only validate repository component versions",
    )
    args = parser.parse_args()
    try:
        if args.check:
            version = component_version(ROOT)
        else:
            version = build_version(
                ROOT,
                os.environ.get("GITHUB_REF_TYPE", ""),
                os.environ.get("GITHUB_REF_NAME", ""),
                os.environ.get("GITHUB_SHA", ""),
            )
    except (OSError, ValueError, KeyError, TypeError) as exc:
        print(f"release version: {exc}", file=sys.stderr)
        return 1
    print(f"version={version}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
