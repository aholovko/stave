"""Manifest and file checks use real tokenizer fixtures and dummy model bytes.

Loading is tested separately; these checks must identify the offending manifest field.
"""

import copy
import hashlib
import json
import re
import shutil
from pathlib import Path
from typing import Any

import pytest

from stave_infer import bundle

FIXTURES = Path(__file__).parent / "fixtures"
TOKENIZER = FIXTURES / "tokenizer"
TASKS = ["P1", "P2", "P3", "TF", "DC", "RP"]
MODEL = b"stand-in model bytes"


def _sha(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def _document(directory: Path) -> dict[str, Any]:
    config = (directory / "backbone-config.json").read_bytes()
    return {
        "format": "stave-model-bundle-1",
        "id": "S-20261010",
        "variant": "S",
        "version": "2026.10.10",
        "description": "test bundle",
        "tasks": TASKS,
        "seed": 20261004,
        "thresholds": {t: 0.0 for t in TASKS},
        "batch_size": 2,
        "input_budgets": {t: 1023 if t in ("P1", "P2", "P3") else 512 for t in TASKS},
        "model": {
            "file": "model.pt",
            "format": "stave-structural-6",
            "sha256": _sha(MODEL),
            "bytes": len(MODEL),
        },
        "backbone": {
            "name": "microsoft/unixcoder-base",
            "revision": "5604afdc964f6c53782a6813140ade5216b99006",
            "config": "backbone-config.json",
            "config_sha256": _sha(config),
            "weight_sha256": "6732b60037b086557e4c731c3748c99b4fec735d9ffe3c4c70061fd142e6b9d9",
            "weight_bytes": 503809527,
            "parameters": 125929728,
        },
        "tokenizer": {
            "dir": "tokenizer",
            "revision": "5604afdc964f6c53782a6813140ade5216b99006",
            "files": {
                name: _sha((directory / "tokenizer" / name).read_bytes())
                for name in bundle.TOKENIZER_FILES
            },
        },
        "provenance": {"run": "test", "plan_sha256": "0" * 64},
        "license": "Apache-2.0",
    }


@pytest.fixture
def directory(tmp_path: Path) -> Path:
    root = tmp_path / "bundle"
    shutil.copytree(
        TOKENIZER, root / "tokenizer", ignore=shutil.ignore_patterns("*.md")
    )
    shutil.copy(FIXTURES / "backbone-config.json", root / "backbone-config.json")
    (root / "model.pt").write_bytes(MODEL)
    _write(root, _document(root))
    return root


def _write(directory: Path, document: Any) -> None:
    (directory / "manifest.json").write_text(json.dumps(document))


def _edited(directory: Path, change: Any) -> None:
    document = json.loads((directory / "manifest.json").read_text())
    change(document)
    _write(directory, document)


def test_a_valid_manifest_reads_and_its_files_verify(directory: Path) -> None:
    m = bundle.read_manifest(directory)

    assert (m.id, m.variant, m.version) == ("S-20261010", "S", "2026.10.10")
    assert m.tasks == tuple(TASKS) == bundle.TASKS
    assert m.seed == 20261004 and m.batch_size == 2
    assert m.thresholds == {t: 0.0 for t in TASKS}
    assert m.input_budgets["P1"] == 1023 and m.input_budgets["TF"] == 512
    assert m.model.format == "stave-structural-6" and m.model.bytes == len(MODEL)
    assert m.backbone.parameters == 125929728
    assert set(m.tokenizer.files) == set(bundle.TOKENIZER_FILES)
    bundle.verify_files(directory, m)


def test_the_runtime_declares_its_formats_tasks_and_budget_limit() -> None:
    assert bundle.FORMAT == "stave-model-bundle-1"
    assert bundle.SUPPORTED_MODEL_FORMATS == {
        "stave-flat-1": ("B",),
        "stave-structural-6": ("F", "S"),
    }
    assert bundle.TASKS == ("P1", "P2", "P3", "TF", "DC", "RP")
    assert bundle.MAX_BUDGET == 1023


def test_every_variant_pairs_only_with_its_model_format(directory: Path) -> None:
    for variant, model_format in (("B", "stave-flat-1"), ("F", "stave-structural-6")):
        _edited(directory, lambda d: (d.update(variant=variant),
                                      d["model"].update(format=model_format)))  # fmt: skip
        assert bundle.read_manifest(directory).variant == variant


def _set(path: str, value: Any) -> Any:
    keys = path.split("/")

    def change(document: dict[str, Any]) -> None:
        target = document
        for key in keys[:-1]:
            target = target[key]
        target[keys[-1]] = value

    return change


def _drop(path: str) -> Any:
    keys = path.split("/")

    def change(document: dict[str, Any]) -> None:
        target = document
        for key in keys[:-1]:
            target = target[key]
        del target[keys[-1]]

    return change


@pytest.mark.parametrize(
    ("change", "field"),
    [
        pytest.param(_set("format", "stave-model-bundle-2"), "format", id="format"),
        pytest.param(_set("model/format", "stave-structural-7"), "model.format",
                     id="model-format"),
        pytest.param(_set("variant", "B"), "variant", id="variant-for-format"),
        pytest.param(_set("variant", "X"), "variant", id="unknown-variant"),
        pytest.param(_set("tasks", TASKS[::-1]), "tasks", id="tasks-order"),
        pytest.param(_set("tasks", TASKS[:5]), "tasks", id="tasks-missing"),
        pytest.param(_set("tasks", ["E1", "E2", "E5", "TF", "DC", "RP"]),
                     "tasks", id="tasks-legacy"),
        pytest.param(_set("tasks", ["P1", "E2", "P3", "TF", "DC", "RP"]),
                     "tasks", id="tasks-mixed"),
        pytest.param(_set("thresholds", {**dict.fromkeys(TASKS, 0.0), "E1": 0.0}),
                     "thresholds", id="threshold-legacy"),
        pytest.param(_set("input_budgets", {**dict.fromkeys(TASKS, 512), "E5": 512}),
                     "input_budgets", id="budget-legacy"),
        pytest.param(_set("seed", "20261004"), "seed", id="seed-string"),
        pytest.param(_set("seed", True), "seed", id="seed-bool"),
        pytest.param(_set("thresholds/P1", "0"), "thresholds", id="threshold-string"),
        pytest.param(_drop("thresholds/RP"), "thresholds", id="threshold-missing"),
        pytest.param(_set("batch_size", 0), "batch_size", id="batch-size"),
        pytest.param(_set("input_budgets/P1", 1024), "input_budgets", id="budget-over"),
        pytest.param(_set("input_budgets/TF", 0), "input_budgets", id="budget-zero"),
        pytest.param(_drop("input_budgets/DC"), "input_budgets", id="budget-missing"),
        pytest.param(_set("id", "../S"), "id", id="id-path"),
        pytest.param(_set("id", ""), "id", id="id-empty"),
        pytest.param(_set("model/file", "../model.pt"), "model.file", id="file-escapes"),
        pytest.param(_set("model/file", "/model.pt"), "model.file", id="file-absolute"),
        pytest.param(_set("model/sha256", "A" * 64), "model.sha256", id="sha-case"),
        pytest.param(_set("model/bytes", -1), "model.bytes", id="bytes-negative"),
        pytest.param(_drop("tokenizer/files/merges.txt"), "tokenizer.files",
                     id="tokenizer-file-unlisted"),
        pytest.param(_set("tokenizer/files/extra.json", "0" * 64), "tokenizer.files",
                     id="tokenizer-file-extra"),
        pytest.param(_drop("backbone/parameters"), "backbone", id="field-missing"),
        pytest.param(_set("backbone/parameters", 0), "backbone.parameters",
                     id="parameters-zero"),
        pytest.param(_set("unknown", 1), "unknown", id="field-unknown"),
    ],
)  # fmt: skip
def test_each_manifest_refusal_names_its_field(
    directory: Path, change: Any, field: str
) -> None:
    _edited(directory, change)
    with pytest.raises(bundle.BundleError, match=re.escape(field)):
        bundle.read_manifest(directory)


def test_a_manifest_that_is_not_a_json_object_is_refused(directory: Path) -> None:
    (directory / "manifest.json").write_text("[]")
    with pytest.raises(bundle.BundleError, match="manifest"):
        bundle.read_manifest(directory)
    (directory / "manifest.json").write_text("{")
    with pytest.raises(bundle.BundleError, match="manifest"):
        bundle.read_manifest(directory)
    (directory / "manifest.json").unlink()
    with pytest.raises(bundle.BundleError, match="manifest"):
        bundle.read_manifest(directory)


@pytest.mark.parametrize(
    ("damage", "field"),
    [
        pytest.param(lambda d: (d / "model.pt").write_bytes(MODEL + b"x"), "model.bytes",
                     id="model-size"),
        pytest.param(lambda d: (d / "model.pt").write_bytes(MODEL[::-1]), "model.sha256",
                     id="model-hash"),
        pytest.param(lambda d: (d / "model.pt").unlink(), "model.file", id="model-absent"),
        pytest.param(lambda d: (d / "backbone-config.json").write_text("{}"),
                     "backbone.config_sha256", id="config-hash"),
        pytest.param(lambda d: (d / "tokenizer" / "merges.txt").write_bytes(b"#\n"),
                     "tokenizer.files['merges.txt']", id="tokenizer-hash"),
        pytest.param(lambda d: (d / "tokenizer" / "vocab.json").unlink(),
                     "tokenizer.files['vocab.json']", id="tokenizer-absent"),
    ],
)  # fmt: skip
def test_verify_files_refuses_each_size_or_hash_mismatch(
    directory: Path, damage: Any, field: str
) -> None:
    m = bundle.read_manifest(directory)
    damage(directory)
    with pytest.raises(bundle.BundleError, match=re.escape(field)):
        bundle.verify_files(directory, m)


def test_the_manifest_is_a_frozen_value(directory: Path) -> None:
    m = bundle.read_manifest(directory)
    with pytest.raises(AttributeError):
        m.id = "other"  # type: ignore[misc]
    assert copy.deepcopy(m) == m
