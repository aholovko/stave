"""Release version policy tests, using small independent metadata fixtures."""

import json
import tempfile
import unittest
from pathlib import Path

from version import build_version, component_version

SHA = "1234567890abcdef1234567890abcdef12345678"


class VersionTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        (self.root / "editors/vscode").mkdir(parents=True)
        (self.root / "infer").mkdir()
        self.write_versions()

    def write_versions(self, version="0.1.0", **overrides):
        versions = dict.fromkeys(
            ("extension", "npm", "npm_root", "python", "uv"), version
        )
        versions.update(overrides)
        (self.root / "editors/vscode/package.json").write_text(
            json.dumps({"version": versions["extension"]}), encoding="utf-8"
        )
        (self.root / "editors/vscode/package-lock.json").write_text(
            json.dumps(
                {
                    "version": versions["npm"],
                    "packages": {"": {"version": versions["npm_root"]}},
                }
            ),
            encoding="utf-8",
        )
        (self.root / "infer/pyproject.toml").write_text(
            f'[project]\nname = "stave-infer"\nversion = "{versions["python"]}"\n',
            encoding="utf-8",
        )
        (self.root / "infer/uv.lock").write_text(
            f'[[package]]\nname = "stave-infer"\nversion = "{versions["uv"]}"\n',
            encoding="utf-8",
        )

    def test_stable_release_tags_match_component_versions(self):
        for value in ("0.1.0", "1.2.3"):
            with self.subTest(version=value):
                self.write_versions(value)
                self.assertEqual(component_version(self.root), value)
                self.assertEqual(
                    build_version(self.root, "tag", f"v{value}", SHA), f"v{value}"
                )

    def test_malformed_and_prerelease_tags_are_rejected(self):
        for tag in (
            "",
            "1.2.3",
            "v1.2",
            "v1.2.3.4",
            "v01.2.3",
            "v1.02.3",
            "v1.2.03",
            "v1.2.3-rc.1",
            "v1.2.3+build.1",
            "v1.2.3\n",
            "v١.2.3",
        ):
            with (
                self.subTest(tag=tag),
                self.assertRaisesRegex(ValueError, "stable vX.Y.Z"),
            ):
                build_version(self.root, "tag", tag, SHA)

    def test_tag_must_match_components(self):
        with self.assertRaisesRegex(
            ValueError, "tag v0.2.0 does not match component version 0.1.0"
        ):
            build_version(self.root, "tag", "v0.2.0", SHA)

    def test_each_manifest_and_lock_version_must_match(self):
        for field in ("extension", "npm", "npm_root", "python", "uv"):
            with self.subTest(field=field):
                self.write_versions(**{field: "0.2.0"})
                with self.assertRaisesRegex(ValueError, "does not match"):
                    component_version(self.root)

    def test_component_versions_must_be_stable(self):
        for value in ("01.2.3", "1.2", "1.2.3-rc.1", "1.2.3+build.1"):
            with self.subTest(version=value):
                self.write_versions(value)
                with self.assertRaisesRegex(ValueError, "stable X.Y.Z"):
                    component_version(self.root)

    def test_uv_lock_must_name_the_runtime(self):
        (self.root / "infer/uv.lock").write_text(
            '[[package]]\nname = "another-package"\nversion = "0.1.0"\n',
            encoding="utf-8",
        )
        with self.assertRaisesRegex(ValueError, "exactly one stave-infer"):
            component_version(self.root)

    def test_branch_builds_use_commit_identity(self):
        self.assertEqual(build_version(self.root, "branch", "main", SHA), "dev-1234567")

    def test_branch_builds_still_require_consistent_versions(self):
        self.write_versions(uv="0.2.0")
        with self.assertRaisesRegex(ValueError, "does not match"):
            build_version(self.root, "branch", "main", SHA)

    def test_missing_ref_or_commit_fails(self):
        with self.assertRaisesRegex(ValueError, "GITHUB_REF_TYPE"):
            build_version(self.root, "", "", SHA)
        for sha in ("", "123456", "not-a-commit", SHA + "\n"):
            with (
                self.subTest(sha=sha),
                self.assertRaisesRegex(ValueError, "GITHUB_SHA"),
            ):
                build_version(self.root, "branch", "main", sha)


if __name__ == "__main__":
    unittest.main()
