"""Variant loading and input isolation.

Converted research configurations must match native model configurations. B's saved `init.sha256` remains source
provenance; parameter names and CPU-specific random draws can change the native initialization fingerprint.
"""

import json
from pathlib import Path
from typing import Any

import pytest
import torch
from support import (
    FIXTURES,
    SIX,
    TF_SRC,
    expected_for,
    projections,
    tool,
    toy_backbone,
)

from stave_infer import backbone as bb
from stave_infer import flat as fm
from stave_infer import structural as sm
from stave_infer import variants

RESEARCH = json.loads((FIXTURES / "research-config-json.json").read_text())
checkpoint_ids = tool("bundle/checkpoint_ids.py")


@pytest.mark.parametrize(
    "case", RESEARCH["cases"], ids=lambda c: f"hidden{c['backbone']['hidden_size']}"
)
def test_converted_research_config_matches_native_config_except_the_init_digest(
    case: dict[str, Any],
) -> None:
    expected = bb.Expected(revision=RESEARCH["revision"], **RESEARCH["expected"])
    backbone = toy_backbone(**case["backbone"])
    structural = sm.StructuralModel(backbone, tasks=SIX, seed=case["seed"])
    flat = fm.FlatModel(backbone, tasks=SIX, seed=case["seed"])
    expected_structural = checkpoint_ids.convert_config(json.loads(case["structural"]))
    assert expected_structural["tasks"] == list(SIX)
    assert sm.config_json(structural, expected) == json.dumps(
        expected_structural, sort_keys=True
    )
    original_flat = json.loads(case["flat"])
    expected_flat = checkpoint_ids.convert_config(original_flat)
    assert expected_flat["init"]["sha256"] == original_flat["init"]["sha256"]
    expected_flat["init"]["sha256"] = fm.digest(flat)
    assert fm.config_json(flat, expected) == json.dumps(expected_flat, sort_keys=True)


def _saved(tmp_path: Path, arm: str, seed: int = 11) -> tuple[Path, bb.Expected]:
    backbone = toy_backbone()
    expected = expected_for(backbone)
    path = tmp_path / f"{arm}.pt"
    if arm == "B":
        fm.save(fm.FlatModel(backbone, tasks=SIX, seed=seed), path, expected)
    else:
        sm.save(sm.StructuralModel(backbone, tasks=SIX, seed=seed), path, expected)
    return path, expected


@pytest.mark.parametrize("arm", variants.ARMS)
def test_each_arm_loads_its_own_model_checked_against_seed_and_tasks(
    tmp_path: Path, arm: str
) -> None:
    assert variants.ARMS == ("B", "F", "S")
    path, expected = _saved(tmp_path, arm)
    model = variants.load_model(arm, path, toy_backbone(), expected, SIX, 11)
    assert isinstance(model, fm.FlatModel if arm == "B" else sm.StructuralModel)
    assert not model.training
    with pytest.raises(variants.VariantError, match="seed"):
        variants.load_model(arm, path, toy_backbone(), expected, SIX, 12)
    with pytest.raises(variants.VariantError, match="inventory"):
        variants.load_model(arm, path, toy_backbone(), expected, SIX[:5], 11)


@pytest.mark.parametrize("arm", ("B", "S"))
@pytest.mark.parametrize(
    "tasks",
    [
        ("E1", "E2", "E5", "TF", "DC", "RP"),
        ("P1", "E2", "P3", "TF", "DC", "RP"),
    ],
    ids=("legacy", "mixed"),
)
def test_runtime_rejects_legacy_and_mixed_checkpoint_tasks(
    tmp_path: Path, arm: str, tasks: tuple[str, ...]
) -> None:
    path, expected = _saved(tmp_path, arm)
    saved = torch.load(path, weights_only=True)
    config = json.loads(saved["config_json"])
    config["tasks"] = list(tasks)
    if arm == "B":
        config["init"]["tasks"] = list(tasks)
    saved["config_json"] = json.dumps(config, sort_keys=True)
    torch.save(saved, path)
    with pytest.raises(variants.VariantError, match="canonical subsequence"):
        variants.load_model(arm, path, toy_backbone(), expected, SIX, 11)


def test_a_model_of_the_other_family_is_refused_by_format(tmp_path: Path) -> None:
    flat, expected = _saved(tmp_path, "B")
    structural, _ = _saved(tmp_path, "S")
    with pytest.raises(variants.VariantError, match="format"):
        variants.load_model("S", flat, toy_backbone(), expected, SIX, 11)
    with pytest.raises(variants.VariantError, match="format"):
        variants.load_model("B", structural, toy_backbone(), expected, SIX, 11)
    with pytest.raises(variants.VariantError, match="arm"):
        variants.load_model("X", structural, toy_backbone(), expected, SIX, 11)


def test_a_backbone_with_another_parameter_count_is_refused(tmp_path: Path) -> None:
    path, expected = _saved(tmp_path, "F")
    wider = toy_backbone(intermediate_size=64)
    with pytest.raises(variants.VariantError, match="parameters"):
        variants.load_model("F", path, wider, expected, SIX, 11)


def test_unreadable_and_non_strict_states_are_variant_errors(tmp_path: Path) -> None:
    path, expected = _saved(tmp_path, "S")
    (tmp_path / "junk.pt").write_bytes(b"not a torch file")
    with pytest.raises(variants.VariantError, match="junk.pt"):
        variants.load_model(
            "S", tmp_path / "junk.pt", toy_backbone(), expected, SIX, 11
        )
    with pytest.raises(variants.VariantError, match="absent.pt"):
        variants.load_model(
            "S", tmp_path / "absent.pt", toy_backbone(), expected, SIX, 11
        )
    saved = torch.load(path, weights_only=True)
    del saved["state"]["heads.RP.empty"]
    torch.save(saved, tmp_path / "missing.pt")
    with pytest.raises(variants.VariantError, match="heads.RP.empty"):
        variants.load_model(
            "S", tmp_path / "missing.pt", toy_backbone(), expected, SIX, 11
        )


def test_b_admits_an_incomplete_projection_that_f_and_s_withhold() -> None:
    incomplete = projections("TF", TF_SRC)["TF:x:0"]
    assert incomplete.status == "incomplete"
    assert variants.admit("B", incomplete) is None
    for arm in ("F", "S"):
        reason = variants.admit(arm, incomplete)
        assert reason is not None and reason.startswith("incomplete: required roles")
    admitted = projections("TF", TF_SRC)["TF:x:1"]
    for arm in variants.ARMS:
        assert variants.admit(arm, admitted) is None
    batches = {arm: variants.collate(arm, [admitted]) for arm in variants.ARMS}
    assert isinstance(batches["B"], fm.FlatBatch)
    assert isinstance(batches["F"], sm.Batch) and isinstance(batches["S"], sm.Batch)
    assert not torch.equal(batches["F"].masks, batches["S"].masks)
