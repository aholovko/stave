"""Bundle packaging from a toy training run.

The run has real receipts and model files so its output can pass the runtime's compatibility check.
"""

import hashlib
import json
import os
import shutil
import subprocess
import sys
import zipfile
from pathlib import Path
from typing import Any

import pytest
import torch
from support import (
    REPO,
    TOKENIZER_DIR,
    TOKENIZER_NAMES,
    copy_fixture_package,
    go_stave,
    selector,
    tool,
)
from test_checkpoint_ids import legacy_checkpoint

from stave_infer import __main__ as cli
from stave_infer import backend, bundle, serving, toy

make_bundle = tool("bundle/make_bundle.py")

SIX = list(bundle.TASKS)


def _sha256(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


def toy_run(tmp: Path, arm: str) -> dict[str, Path]:
    """Make a run directory that holds a toy terminal model of `arm`, with its inputs."""
    made = tmp / "made"
    m = toy.make_toy_bundle(arm, made, tokenizer_dir=TOKENIZER_DIR)
    run = tmp / "toy-training-2026-10-10" / "execution-001" / "run"
    (run / "terminal" / arm).mkdir(parents=True)
    shutil.copyfile(made / "model.pt", run / "terminal" / arm / "model.pt")
    plan = {
        "format": "stave-comparison-plan-1",
        "arms": ["B", "F", "S"],
        "tasks": SIX,
        "thresholds": {t: 0.0 for t in sorted(SIX)},
        "eval_batch_size": 2,
        "init_seed": m.seed,
        "train_seed": m.seed,
        "threads": 12,
        "updates": [[0, 1]] * 3,
    }
    (run / "plan.json").write_text(json.dumps(plan))
    receipt = {
        "format": "stave-comparison-receipt-1",
        "status": "complete",
        "arms": {a: {"status": "complete", "error": None} for a in ("B", "F", "S")},
        "expected": {
            "config_sha256": m.backbone.config_sha256,
            "parameters": m.backbone.parameters,
            "versions": {"torch": "2.13.0"},
            "weight_bytes": m.backbone.weight_bytes,
            "weight_sha256": m.backbone.weight_sha256,
        },
        "hashes": {
            "plan.json": _sha256(run / "plan.json"),
            f"terminal/{arm}/model.pt": _sha256(run / "terminal" / arm / "model.pt"),
        },
    }
    (run / "receipt.json").write_text(json.dumps(receipt))
    tokenizer_receipt = tmp / "tokenizer-artifacts.json"
    tokenizer_receipt.write_text(
        json.dumps(
            {
                "model": m.backbone.name,
                "revision": m.backbone.revision,
                "files": [
                    {
                        "file": name,
                        "bytes": (TOKENIZER_DIR / name).stat().st_size,
                        "sha256": _sha256(TOKENIZER_DIR / name),
                    }
                    for name in TOKENIZER_NAMES
                ],
            }
        )
    )
    return {
        "run": run,
        "backbone_config": made / "backbone-config.json",
        "tokenizer_dir": TOKENIZER_DIR,
        "tokenizer_receipt": tokenizer_receipt,
    }


def _build(paths: dict[str, Path], arm: str, out: Path, **extra: Any) -> Any:
    return make_bundle.build(
        run=paths["run"],
        arm=arm,
        ident=f"{arm}-20261010",
        version="2026.10.10",
        backbone_config=paths["backbone_config"],
        tokenizer_dir=paths["tokenizer_dir"],
        tokenizer_receipt=paths["tokenizer_receipt"],
        out=out,
        **extra,
    )


@pytest.mark.parametrize(
    "arm, model_format",
    [("B", "stave-flat-1"), ("F", "stave-structural-6"), ("S", "stave-structural-6")],
)
def test_the_bundle_carries_the_runs_identity_and_passes_check_bundle(
    tmp_path: Path, arm: str, model_format: str
) -> None:
    paths = toy_run(tmp_path, arm)
    built = _build(paths, arm, tmp_path / "out")
    directory = tmp_path / "out" / f"{arm}-20261010"
    assert built.id == f"{arm}-20261010"
    assert built.directory == directory
    assert built.zip == tmp_path / "out" / f"{arm}-20261010.zip"

    manifest = json.loads((directory / "manifest.json").read_text())
    receipt = json.loads((paths["run"] / "receipt.json").read_text())
    expected = receipt["expected"]
    assert manifest["format"] == "stave-model-bundle-1"
    assert manifest["variant"] == arm
    assert manifest["version"] == "2026.10.10"
    assert manifest["tasks"] == SIX
    assert manifest["seed"] == toy.SEED
    assert manifest["thresholds"] == {t: 0.0 for t in SIX}
    assert manifest["batch_size"] == 2
    assert manifest["input_budgets"] == {
        "P1": 1023, "P2": 1023, "P3": 1023, "TF": 512, "DC": 512, "RP": 512,
    }  # fmt: skip
    model = paths["run"] / "terminal" / arm / "model.pt"
    assert manifest["model"] == {
        "file": "model.pt",
        "format": model_format,
        "sha256": receipt["hashes"][f"terminal/{arm}/model.pt"],
        "bytes": model.stat().st_size,
    }
    assert manifest["backbone"] == {
        "name": "stave-toy-roberta",
        "revision": "toy",
        "config": "backbone-config.json",
        "config_sha256": expected["config_sha256"],
        "weight_sha256": expected["weight_sha256"],
        "weight_bytes": expected["weight_bytes"],
        "parameters": expected["parameters"],
    }
    assert manifest["tokenizer"] == {
        "dir": "tokenizer",
        "revision": "toy",
        "files": {n: _sha256(TOKENIZER_DIR / n) for n in TOKENIZER_NAMES},
    }
    assert manifest["provenance"] == {
        "run": "toy-training-2026-10-10/execution-001",
        "plan_sha256": receipt["hashes"]["plan.json"],
    }
    assert manifest["license"] == "Apache-2.0"
    assert manifest["description"]
    assert (directory / "model.pt").read_bytes() == model.read_bytes()
    assert (directory / "LICENSE").read_bytes() == (REPO / "LICENSE").read_bytes()
    assert (directory / "NOTICE").read_bytes() == (REPO / "NOTICE").read_bytes()

    assert cli.main(["check-bundle", str(directory)]) == 0

    with zipfile.ZipFile(built.zip) as z:
        names = sorted(z.namelist())
        assert all(z.read(n) == (tmp_path / "out" / n).read_bytes() for n in names)
    assert names == sorted(
        f"{arm}-20261010/{n}"
        for n in (
            "manifest.json", "model.pt", "backbone-config.json", "LICENSE", "NOTICE",
            *(f"tokenizer/{t}" for t in TOKENIZER_NAMES),
        )
    )  # fmt: skip
    assert built.size == built.zip.stat().st_size
    assert built.sha256 == _sha256(built.zip)


def test_the_description_is_taken_when_given(tmp_path: Path) -> None:
    paths = toy_run(tmp_path, "S")
    built = _build(paths, "S", tmp_path / "out", description="the demonstrated S")
    manifest = json.loads((built.directory / "manifest.json").read_text())
    assert manifest["description"] == "the demonstrated S"


def test_the_same_inputs_give_the_same_zip(tmp_path: Path) -> None:
    paths = toy_run(tmp_path, "B")
    first = _build(paths, "B", tmp_path / "one")
    second = _build(paths, "B", tmp_path / "two")
    assert first.sha256 == second.sha256


def _research_run(paths: dict[str, Path], arm: str) -> dict[str, bytes]:
    """Convert a test run to the old task names and pin those source artifacts in its receipt."""
    run = paths["run"]
    model = run / "terminal" / arm / "model.pt"
    saved = legacy_checkpoint(torch.load(model, weights_only=True))
    torch.save(saved, model)
    plan = json.loads((run / "plan.json").read_text())
    tasks = json.loads(saved["config_json"])["tasks"]
    plan["thresholds"] = dict(
        zip(tasks, (plan["thresholds"][t] for t in plan["tasks"]), strict=True)
    )
    plan["tasks"] = tasks
    (run / "plan.json").write_text(json.dumps(plan))
    receipt = json.loads((run / "receipt.json").read_text())
    receipt["hashes"]["plan.json"] = _sha256(run / "plan.json")
    receipt["hashes"][f"terminal/{arm}/model.pt"] = _sha256(model)
    (run / "receipt.json").write_text(json.dumps(receipt))
    return {
        str(p): p.read_bytes() for p in (model, run / "plan.json", run / "receipt.json")
    }


@pytest.mark.parametrize("arm", ("B", "F", "S"))
def test_legacy_packaging_preserves_predictions_and_records_conversion(
    tmp_path: Path, arm: str
) -> None:
    paths = toy_run(tmp_path, arm)
    native_dir = paths["backbone_config"].parent
    native_manifest = bundle.read_manifest(native_dir)
    originals = _research_run(paths, arm)
    built = _build(paths, arm, tmp_path / "out")
    manifest = bundle.read_manifest(built.directory)
    raw = json.loads((built.directory / "manifest.json").read_text())
    source_model = paths["run"] / "terminal" / arm / "model.pt"
    assert manifest.tasks == tuple(SIX)
    assert raw["provenance"]["source_model_sha256"] == _sha256(source_model)
    assert raw["provenance"]["source_tasks"] == ["E1", "E2", "E5", "TF", "DC", "RP"]
    assert raw["provenance"]["conversion"] == make_bundle.checkpoint_ids.CONVERSION
    assert raw["model"]["sha256"] == _sha256(built.directory / "model.pt")
    assert raw["model"]["sha256"] != raw["provenance"]["source_model_sha256"]
    if arm == "B":
        assert (
            "source initialization fingerprint" in raw["provenance"]["init_sha256_note"]
        )
    assert all(Path(path).read_bytes() == data for path, data in originals.items())
    again = _build(paths, arm, tmp_path / "again")
    assert again.sha256 == built.sha256

    home = tmp_path / "home"
    installed = subprocess.run(
        [str(go_stave()), "model", "add", str(built.zip)],
        env={
            **os.environ,
            "STAVE_HOME": str(home),
            "STAVE_RUNTIME_PYTHON": sys.executable,
        },
        capture_output=True,
        text=True,
        timeout=600,
        check=False,
    )
    assert installed.returncode == 0, installed.stderr
    assert f"active model: {built.id}" in installed.stdout
    assert (
        bundle.read_manifest(home / "models" / built.id).provenance == raw["provenance"]
    )

    pkg = copy_fixture_package(tmp_path / "inputs")
    request = json.dumps(
        {
            "schema": "stave-advisory-v1",
            "files": [
                {"path": str(p), "sha256": _sha256(p)} for p in sorted(pkg.iterdir())
            ],
        }
    ).encode()
    native = serving.load(native_dir, native_manifest, threads=1)
    converted = serving.load(built.directory, manifest, threads=1)
    expected = backend.respond(
        request, loaded=native, manifest=native_manifest, selector=selector()
    )
    actual = backend.respond(
        request, loaded=converted, manifest=manifest, selector=selector()
    )
    assert actual["classes"] == expected["classes"]
    assert actual["files"] == expected["files"]
    assert actual["predictions"] == expected["predictions"]


def test_a_tampered_legacy_model_is_refused_before_loading(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    paths = toy_run(tmp_path, "S")
    _research_run(paths, "S")
    _tamper_model(paths)

    def refuse_load(*args: Any, **kwargs: Any) -> None:
        pytest.fail("a model that does not match its receipt must not be loaded")

    monkeypatch.setattr(torch, "load", refuse_load)
    with pytest.raises(make_bundle.BuildError, match="sha256"):
        _build(paths, "S", tmp_path / "out")


def test_a_receipt_pinned_but_truncated_checkpoint_fails_cleanly(
    tmp_path: Path, capsys: pytest.CaptureFixture[str]
) -> None:
    paths = toy_run(tmp_path, "S")
    model = paths["run"] / "terminal" / "S" / "model.pt"
    model.write_bytes(model.read_bytes()[:-128])
    receipt_path = paths["run"] / "receipt.json"
    receipt = json.loads(receipt_path.read_text())
    receipt["hashes"]["terminal/S/model.pt"] = _sha256(model)
    receipt_path.write_text(json.dumps(receipt))
    out = tmp_path / "out"
    argv = [
        "--run", str(paths["run"]), "--arm", "S", "--id", "S-20261010",
        "--version", "2026.10.10", "--backbone-config", str(paths["backbone_config"]),
        "--tokenizer-dir", str(paths["tokenizer_dir"]),
        "--tokenizer-receipt", str(paths["tokenizer_receipt"]), "--out", str(out),
    ]  # fmt: skip
    assert make_bundle.main(argv) == 1
    captured = capsys.readouterr()
    assert captured.out == ""
    assert captured.err.startswith("make_bundle: ") and str(model) in captured.err
    assert captured.err.count("\n") == 1 and "Traceback" not in captured.err
    assert not (out / "S-20261010").exists()
    assert not (out / "S-20261010.zip").exists()


def test_a_mixed_plan_inventory_is_refused(tmp_path: Path) -> None:
    paths = toy_run(tmp_path, "S")
    plan_path = paths["run"] / "plan.json"
    plan = json.loads(plan_path.read_text())
    plan["tasks"][0] = "E1"
    plan_path.write_text(json.dumps(plan))
    receipt_path = paths["run"] / "receipt.json"
    receipt = json.loads(receipt_path.read_text())
    receipt["hashes"]["plan.json"] = _sha256(plan_path)
    receipt_path.write_text(json.dumps(receipt))
    with pytest.raises(make_bundle.BuildError, match="mixed"):
        _build(paths, "S", tmp_path / "out")


def _tamper_model(paths: dict[str, Path]) -> None:
    model = next((paths["run"] / "terminal").glob("*/model.pt"))
    data = bytearray(model.read_bytes())
    data[-1] ^= 1
    model.write_bytes(bytes(data))


def _tamper_plan(paths: dict[str, Path]) -> None:
    plan = json.loads((paths["run"] / "plan.json").read_text())
    plan["thresholds"]["TF"] = 0.5
    (paths["run"] / "plan.json").write_text(json.dumps(plan))


def _tamper_config(paths: dict[str, Path]) -> None:
    config = paths["backbone_config"]
    config.write_text(config.read_text() + " ")


def _tamper_tokenizer(paths: dict[str, Path]) -> None:
    receipt = json.loads(paths["tokenizer_receipt"].read_text())
    receipt["files"][1]["sha256"] = "0" * 64
    paths["tokenizer_receipt"].write_text(json.dumps(receipt))


def _incomplete_arm(paths: dict[str, Path]) -> None:
    receipt = json.loads((paths["run"] / "receipt.json").read_text())
    receipt["arms"]["S"]["status"] = "failed"
    (paths["run"] / "receipt.json").write_text(json.dumps(receipt))


@pytest.mark.parametrize(
    "tamper, named",
    [
        (_tamper_model, "terminal/S/model.pt"),
        (_tamper_plan, "plan.json"),
        (_tamper_config, "config_sha256"),
        (_tamper_tokenizer, "merges.txt"),
        (_incomplete_arm, "arm S"),
    ],
)
def test_inputs_that_do_not_match_the_receipts_are_refused(
    tmp_path: Path, tamper: Any, named: str
) -> None:
    paths = toy_run(tmp_path, "S")
    tamper(paths)
    with pytest.raises(make_bundle.BuildError) as error:
        _build(paths, "S", tmp_path / "out")
    assert named in str(error.value).replace("\\", "/")
    assert not (tmp_path / "out" / "S-20261010.zip").exists()


def test_an_existing_bundle_is_not_overwritten(tmp_path: Path) -> None:
    paths = toy_run(tmp_path, "S")
    (tmp_path / "out" / "S-20261010").mkdir(parents=True)
    with pytest.raises(make_bundle.BuildError, match="exists"):
        _build(paths, "S", tmp_path / "out")


def test_the_command_prints_the_id_zip_size_and_sha256(
    tmp_path: Path, capsys: pytest.CaptureFixture[str]
) -> None:
    paths = toy_run(tmp_path, "S")
    argv = [
        "--run", str(paths["run"]), "--arm", "S", "--id", "S-20261010",
        "--version", "2026.10.10", "--backbone-config", str(paths["backbone_config"]),
        "--tokenizer-dir", str(paths["tokenizer_dir"]),
        "--tokenizer-receipt", str(paths["tokenizer_receipt"]),
        "--out", str(tmp_path / "out"),
    ]  # fmt: skip
    assert make_bundle.main(argv) == 0
    archive = tmp_path / "out" / "S-20261010.zip"
    out = capsys.readouterr().out.split()
    assert out == [
        "S-20261010",
        str(archive),
        str(archive.stat().st_size),
        _sha256(archive),
    ]
    assert make_bundle.main(argv) == 1
    err = capsys.readouterr().err
    assert err.startswith("make_bundle: ") and err.count("\n") == 1
