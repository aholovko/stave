"""`python -m stave_infer serve | check-bundle | version` as the Go side runs them.

Stdout carries the response only. Handled runtime failures exit with status 1 and write one line to stderr.
"""

import importlib.metadata
import json
import os
import shutil
import subprocess
import sys
from pathlib import Path
from typing import Any

import pytest
from support import TOKENIZER_DIR, copy_fixture_package, go_stave, sha256

from stave_infer import toy


@pytest.fixture(scope="module")
def bundles(tmp_path_factory: pytest.TempPathFactory) -> dict[str, Path]:
    root = tmp_path_factory.mktemp("bundles")
    for arm in ("B", "F", "S"):
        toy.make_toy_bundle(arm, root / arm, tokenizer_dir=TOKENIZER_DIR)
    return {arm: root / arm for arm in ("B", "F", "S")}


def _run(*args: str, stdin: bytes = b"") -> subprocess.CompletedProcess[bytes]:
    return subprocess.run(
        [sys.executable, "-m", "stave_infer", *args],
        input=stdin,
        capture_output=True,
        timeout=600,
        check=False,
    )


def _request(*paths: Path) -> bytes:
    files = [{"path": str(p), "sha256": sha256(p.read_bytes())} for p in paths]
    return json.dumps({"schema": "stave-advisory-v1", "files": files}).encode()


def _failed(done: subprocess.CompletedProcess[bytes], needle: str) -> None:
    assert done.returncode == 1 and done.stdout == b""
    lines = done.stderr.decode().strip().splitlines()
    assert lines and lines[-1].startswith("stave_infer: ") and needle in lines[-1], (
        lines
    )


def _copy(bundles: dict[str, Path], arm: str, tmp_path: Path) -> Path:
    target = tmp_path / f"copy-{arm}"
    shutil.copytree(bundles[arm], target)
    return target


def _flip(path: Path, at: int = 100) -> None:
    data = bytearray(path.read_bytes())
    data[at] ^= 1
    path.write_bytes(bytes(data))


def _edit(directory: Path, change: Any) -> None:
    path = directory / "manifest.json"
    document = json.loads(path.read_text())
    change(document)
    path.write_text(json.dumps(document))


def test_serve_answers_the_fixture_request_with_the_bundle_identity(
    tmp_path: Path, bundles: dict[str, Path]
) -> None:
    pkg = copy_fixture_package(tmp_path)
    request = _request(pkg / "demo.go", pkg / "demo_test.go", pkg / "external_test.go")
    trace = tmp_path / "trace"
    trace.mkdir()
    done = _run(
        "serve", "--bundle", str(bundles["S"]), "--selector", str(go_stave()),
        "--threads", "2", "--trace-dir", str(trace), stdin=request,
    )  # fmt: skip
    assert done.returncode == 0, done.stderr.decode()
    assert done.stdout.count(b"\n") == 1
    response = json.loads(done.stdout)
    assert response["schema"] == "stave-advisory-v1"
    assert response["backend"]["id"] == "toy-S"
    assert response["backend"]["detail"].startswith("variant S, seed ")
    assert [f["status"] for f in response["files"]] == ["checked"] * 3
    assert {p["class"] for p in response["predictions"]} == {
        "P1", "P2", "P3", "TF", "DC", "RP",
    }  # fmt: skip
    (report,) = trace.iterdir()
    assert json.loads(report.read_text())["threads"]["intra_op"] == 2


def test_serve_refuses_a_malformed_request_before_loading(
    bundles: dict[str, Path], tmp_path: Path
) -> None:
    done = _run(
        "serve", "--bundle", str(tmp_path / "absent"), "--selector", "stave",
        stdin=b'{"schema": "stave-advisory-v1"}',
    )  # fmt: skip
    _failed(done, "'files'")


def test_serve_refuses_a_tampered_bundle(
    bundles: dict[str, Path], tmp_path: Path
) -> None:
    bad = _copy(bundles, "B", tmp_path)
    _flip(bad / "model.pt")
    done = _run(
        "serve", "--bundle", str(bad), "--selector", "stave",
        stdin=json.dumps({"schema": "stave-advisory-v1", "files": []}).encode(),
    )  # fmt: skip
    _failed(done, "model.sha256")


@pytest.mark.parametrize("arm", ["B", "F", "S"])
def test_check_bundle_accepts_each_variant(bundles: dict[str, Path], arm: str) -> None:
    done = _run("check-bundle", str(bundles[arm]))
    assert done.returncode == 0, done.stderr.decode()
    assert done.stdout.decode().startswith(f"toy-{arm}: compatible")


@pytest.mark.parametrize(
    ("arm", "damage", "needle"),
    [
        pytest.param("S", lambda d: _flip(d / "model.pt"), "model.sha256",
                     id="tampered-model"),
        pytest.param("S", lambda d: (d / "tokenizer" / "merges.txt").write_bytes(b"#\n"),
                     "merges.txt", id="tampered-tokenizer"),
        pytest.param("S", lambda d: (d / "tokenizer" / "added_tokens.json").write_text(
            '{"foobar": 10}'), "unpinned entries: added_tokens.json",
            id="unpinned-tokenizer-file"),
        pytest.param("S", lambda d: _edit(d, lambda m: m.update(seed=m["seed"] + 1)),
                     "seed", id="seed"),
        pytest.param("S", lambda d: _edit(d, lambda m: (m.update(variant="B"),
                     m["model"].update(format="stave-flat-1"))), "format",
                     id="structural-model-as-B"),
        pytest.param("B", lambda d: _edit(d, lambda m: (m.update(variant="S"),
                     m["model"].update(format="stave-structural-6"))), "format",
                     id="flat-model-as-S"),
        pytest.param("F", lambda d: _edit(d, lambda m: m["model"].update(
            format="stave-structural-7")), "runtime release", id="newer-format"),
        pytest.param("F", lambda d: _edit(d, lambda m: m["tasks"].pop()), "tasks",
                     id="tasks"),
        pytest.param("F", lambda d: _edit(d, lambda m: m["input_budgets"].update(
            TF=2048)), "input_budgets", id="budget"),
        pytest.param("F", lambda d: _edit(d, lambda m: m["backbone"].update(
            weight_sha256="2" * 64)), "config", id="backbone-identity"),
    ],
)  # fmt: skip
def test_check_bundle_refuses_an_incompatible_bundle_naming_the_mismatch(
    bundles: dict[str, Path], tmp_path: Path, arm: str, damage: Any, needle: str
) -> None:
    bad = _copy(bundles, arm, tmp_path)
    damage(bad)
    _failed(_run("check-bundle", str(bad)), needle)


def test_version_names_the_runtime_and_what_it_supports() -> None:
    done = _run("version")
    assert done.returncode == 0
    line = done.stdout.decode().strip()
    assert line.startswith(f"stave-infer {importlib.metadata.version('stave-infer')} ")
    for word in ("stave-model-bundle-1", "stave-flat-1", "stave-structural-6",
                 "stave-advisory-v1"):  # fmt: skip
        assert word in line


@pytest.mark.skipif(os.name != "nt", reason="Windows path syntax")
def test_serve_round_trip_on_windows_style_paths(
    tmp_path: Path, bundles: dict[str, Path]
) -> None:
    pkg = copy_fixture_package(tmp_path)
    paths = [pkg / "demo.go", pkg / "demo_test.go"]
    assert all("\\" in str(p) for p in paths)
    done = _run(
        "serve", "--bundle", str(bundles["S"]), "--selector", str(go_stave()),
        stdin=_request(*paths),
    )  # fmt: skip
    assert done.returncode == 0, done.stderr.decode()
    response = json.loads(done.stdout)
    assert [f["path"] for f in response["files"]] == [str(p) for p in paths]
    assert [f["status"] for f in response["files"]] == ["checked", "checked"]
    assert {p["path"] for p in response["predictions"]} == {str(p) for p in paths}
