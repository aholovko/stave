"""Toy bundles of B, F and S against recorded fixture outputs.

Spans, coverage and decisions must match exactly. Logits allow 1e-4 variation across platforms, with enough
threshold margin to keep decisions stable. Use `recorded()` to regenerate outputs after an intended change.
"""

import json
import subprocess
import sys
from pathlib import Path
from typing import Any

import pytest
from support import FIXTURES, TOKENIZER_DIR, copy_fixture_package, selector, sha256

from stave_infer import __main__ as cli
from stave_infer import backend, bundle, serving, toy

ARMS = ("B", "F", "S")
EXPECTED = FIXTURES / "toy-expected"
TOLERANCE, MARGIN = 1e-4, 1e-2


@pytest.fixture(scope="module")
def served(tmp_path_factory: pytest.TempPathFactory) -> dict[str, Any]:
    root = tmp_path_factory.mktemp("toy")
    pkg = copy_fixture_package(root)
    paths = sorted(pkg.iterdir())
    request = json.dumps(
        {
            "schema": "stave-advisory-v1",
            "files": [
                {"path": str(p), "sha256": sha256(p.read_bytes())} for p in paths
            ],
        }
    ).encode()
    out: dict[str, Any] = {"pkg": pkg, "bundles": {}, "responses": {}}
    for arm in ARMS:
        directory = root / arm
        manifest = toy.make_toy_bundle(arm, directory, tokenizer_dir=TOKENIZER_DIR)
        loaded = serving.load(directory, manifest, threads=None)
        out["bundles"][arm] = (directory, manifest)
        out["responses"][arm] = backend.respond(
            request, loaded=loaded, manifest=manifest, selector=selector()
        )
    return out


def recorded(response: dict[str, Any], pkg: Path, seed: int) -> dict[str, Any]:
    """Reduce the paths of the response to file names, for comparison across hosts."""

    def name(path: str) -> str:
        return str(Path(path).relative_to(pkg))

    files = []
    for record in response["files"]:
        entry = {k: v for k, v in record.items() if k not in ("path", "sha256")}
        files.append({"file": name(record["path"]), **entry})
    predictions = [
        {
            "file": name(p["path"]),
            "class": p["class"],
            "span": [p["span"]["start"], p["span"]["end"]],
            "coverage": p["coverage"],
            "advice": p["advice"],
            "reason": p["reason"],
            "score": p["score"],
        }
        for p in response["predictions"]
    ]
    return {
        "source": "a copy of testdata/fixture-pkg in a directory without a testdata component",
        "seed": seed,
        "backend": {k: response["backend"][k] for k in ("id", "detail")},
        "classes": response["classes"],
        "files": files,
        "predictions": predictions,
    }


@pytest.mark.parametrize("arm", ARMS)
def test_each_toy_bundle_passes_check_bundle(
    served: dict[str, Any], arm: str, capsys: pytest.CaptureFixture[str]
) -> None:
    directory, manifest = served["bundles"][arm]
    assert manifest.id == f"toy-{arm}" and manifest.seed == toy.SEED
    assert cli.main(["check-bundle", str(directory)]) == 0
    assert capsys.readouterr().out.startswith(f"toy-{arm}: compatible")


@pytest.mark.parametrize("arm", ARMS)
def test_the_fixture_package_has_candidates_for_every_class(
    served: dict[str, Any], arm: str
) -> None:
    predictions = served["responses"][arm]["predictions"]
    names = {(Path(p["path"]).name, p["class"]) for p in predictions}
    for task in ("P1", "P2", "P3", "DC", "RP"):
        assert ("demo.go", task) in names, task
    for test_file in ("demo_test.go", "external_test.go", "incomplete_test.go"):
        assert (test_file, "TF") in names, test_file
    assert all(r["status"] == "checked" for r in served["responses"][arm]["files"])


@pytest.mark.parametrize("arm", ARMS)
def test_every_judged_logit_lies_well_away_from_the_threshold(
    served: dict[str, Any], arm: str
) -> None:
    manifest = served["bundles"][arm][1]
    judged = [
        p for p in served["responses"][arm]["predictions"] if p["coverage"] == "checked"
    ]
    assert judged
    for p in judged:
        assert abs(p["score"] - manifest.thresholds[p["class"]]) >= MARGIN, p
    advice = {p["advice"] for p in judged}
    assert advice == {True, False}, "the toy weights give both decisions"


def test_b_admits_the_incomplete_projection_that_f_and_s_withhold(
    served: dict[str, Any],
) -> None:
    def incomplete(arm: str) -> list[dict[str, Any]]:
        return [
            p
            for p in served["responses"][arm]["predictions"]
            if Path(p["path"]).name == "incomplete_test.go"
        ]

    (b,) = incomplete("B")
    assert b["coverage"] == "checked" and isinstance(b["advice"], bool)
    for arm in ("F", "S"):
        (p,) = incomplete(arm)
        assert p["coverage"] == "unsupported" and p["advice"] is None
        assert p["reason"].startswith("incomplete: required roles without tokens")
        assert "ARGS" in p["reason"] and p["span"] == b["span"]


@pytest.mark.parametrize("arm", ARMS)
def test_outputs_match_the_recorded_reference(served: dict[str, Any], arm: str) -> None:
    got = recorded(served["responses"][arm], served["pkg"], toy.SEED)
    path = EXPECTED / f"{arm}.json"
    want = json.loads(path.read_text())
    assert {k: v for k, v in got.items() if k != "predictions"} == {
        k: v for k, v in want.items() if k != "predictions"
    }
    assert len(got["predictions"]) == len(want["predictions"])
    for g, w in zip(got["predictions"], want["predictions"], strict=True):
        assert {k: v for k, v in g.items() if k != "score"} == {
            k: v for k, v in w.items() if k != "score"
        }
        if w["score"] is None:
            assert g["score"] is None
        else:
            assert abs(g["score"] - w["score"]) <= TOLERANCE, (g, w)


def test_the_toy_command_writes_a_bundle(tmp_path: Path) -> None:
    out = tmp_path / "toy-B"
    done = subprocess.run(
        [sys.executable, "-m", "stave_infer.toy", "--variant", "B", "--out", str(out),
         "--tokenizer-dir", str(TOKENIZER_DIR)],
        capture_output=True, timeout=600, check=False,
    )  # fmt: skip
    assert done.returncode == 0, done.stderr.decode()
    assert done.stdout.decode().startswith("toy-B ")
    manifest = bundle.read_manifest(out)
    assert manifest.variant == "B" and manifest.model.format == "stave-flat-1"
    bundle.verify_files(out, manifest)
    assert {p.name for p in out.iterdir()} == {
        "manifest.json", "model.pt", "backbone-config.json", "tokenizer", "LICENSE",
        "NOTICE",
    }  # fmt: skip
