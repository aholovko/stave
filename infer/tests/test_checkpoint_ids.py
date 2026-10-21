"""Research checkpoint conversion changes names, while native strict loading retains every tensor."""

import copy
import json
from collections import OrderedDict
from pathlib import Path
from typing import Any

import pytest
import torch
from support import SIX, expected_for, tool, toy_backbone

from stave_infer import flat, structural

checkpoint_ids = tool("bundle/checkpoint_ids.py")
_RESEARCH_IDS = {"P1": "E1", "P2": "E2", "P3": "E5"}


def _research_key(name: str) -> str:
    parts = name.split(".")
    if len(parts) > 1 and parts[0] in ("roles", "heads", "cand"):
        parts[1] = _RESEARCH_IDS.get(parts[1], parts[1])
    return ".".join(parts)


def legacy_checkpoint(saved: dict[str, Any]) -> dict[str, Any]:
    """Build a research-format input from a native test checkpoint without changing its tensors."""
    config = json.loads(saved["config_json"])
    config["tasks"] = [_RESEARCH_IDS.get(t, t) for t in config["tasks"]]
    if config["format"] == "stave-flat-1":
        config["slots"] = {
            _RESEARCH_IDS.get(t, t): n for t, n in config["slots"].items()
        }
        config["init"]["tasks"] = config["tasks"].copy()
        # An opaque source fingerprint must survive conversion, even when native tensor names would hash differently.
        config["init"]["sha256"] = "e" * 64
    else:
        config["roles"] = {
            _RESEARCH_IDS.get(t, t): roles for t, roles in config["roles"].items()
        }
        config["init_order"] = [_research_key(n) for n in config["init_order"]]
    state: Any = OrderedDict((_research_key(n), t) for n, t in saved["state"].items())
    if hasattr(saved["state"], "_metadata"):
        state._metadata = OrderedDict(
            (_research_key(n), copy.deepcopy(v))
            for n, v in saved["state"]._metadata.items()
        )
    return {**saved, "config_json": json.dumps(config, sort_keys=True), "state": state}


@pytest.mark.parametrize("arm", ("B", "F", "S"))
def test_conversion_preserves_tensors_order_and_strict_loading(
    tmp_path: Path, arm: str
) -> None:
    module = flat if arm == "B" else structural
    backbone = toy_backbone()
    expected = expected_for(backbone)
    path = tmp_path / "native.pt"
    if arm == "B":
        flat.save(flat.FlatModel(backbone, tasks=SIX, seed=11), path, expected)
    else:
        structural.save(
            structural.StructuralModel(backbone, tasks=SIX, seed=11), path, expected
        )
    native = torch.load(path, weights_only=True)
    source = legacy_checkpoint(native)
    original_config = source["config_json"]
    converted = checkpoint_ids.convert_checkpoint(source)

    assert source["config_json"] == original_config
    assert list(converted["state"]) == list(native["state"])
    assert list(converted["state"]._metadata) == list(native["state"]._metadata)
    for old, renamed in zip(
        source["state"].values(), converted["state"].values(), strict=True
    ):
        assert old is renamed
    path = tmp_path / "converted.pt"
    torch.save(converted, path)
    loaded = module.load(path, toy_backbone(), expected)
    for name, tensor in loaded.state_dict().items():
        original = native["state"][name]
        assert tensor.dtype == original.dtype and tensor.shape == original.shape
        assert torch.equal(tensor, original), name
    if arm == "B":
        assert loaded.saved_init_sha256 == "e" * 64
        assert json.loads(converted["config_json"])["init"]["sha256"] == "e" * 64


def test_config_conversion_is_targeted_and_supports_partial_inventories() -> None:
    source = {
        "format": "stave-structural-3",
        "tasks": ["E2", "TF"],
        "roles": {"E2": ["CAND", "CALL"], "TF": ["CAND"]},
        "init_order": ["roles.E2.weight", "heads.E2.out.weight", "heads.TF.out.weight"],
        "backbone": {"revision": "E2-is-part-of-this-reference"},
    }
    before = copy.deepcopy(source)
    converted = checkpoint_ids.convert_config(source)
    assert source == before
    assert converted["tasks"] == ["P2", "TF"]
    assert converted["backbone"] == source["backbone"]
    assert converted["init_order"] == [
        "roles.P2.weight",
        "heads.P2.out.weight",
        "heads.TF.out.weight",
    ]
    assert checkpoint_ids.convert_config(converted) == converted
    assert (
        checkpoint_ids.state_key("backbone.heads.E2.weight")
        == "backbone.heads.E2.weight"
    )
    assert checkpoint_ids.state_key("heads.E20.weight") == "heads.E20.weight"
    assert checkpoint_ids.state_key("cand.E5") == "cand.P3"


@pytest.mark.parametrize("change", ("tasks", "roles", "init_order", "state", "format"))
def test_malformed_or_mixed_task_fields_are_refused(change: str) -> None:
    config = {
        "format": "stave-structural-2",
        "tasks": ["E1"],
        "roles": {"E1": ["CAND"]},
        "init_order": ["roles.E1.weight"],
    }
    state = {"roles.E1.weight": torch.zeros(1)}
    if change == "tasks":
        config["tasks"] = ["E1", "P2"]
    elif change == "roles":
        config["roles"] = {"P1": ["CAND"]}
    elif change == "init_order":
        config["init_order"] = ["roles.P1.weight"]
    elif change == "state":
        state = {"roles.P1.weight": torch.zeros(1)}
    else:
        config["format"] = "unknown"
    saved = {
        "format": "stave-structural-2",
        "config_json": json.dumps(config),
        "state": state,
    }
    with pytest.raises(ValueError):
        checkpoint_ids.convert_checkpoint(saved)


def test_flat_init_inventory_must_match_its_tasks() -> None:
    config = {
        "format": "stave-flat-1",
        "tasks": ["E1"],
        "slots": {"E1": 6},
        "init": {"tasks": ["P1"], "sha256": "e" * 64},
    }
    with pytest.raises(ValueError, match="init.tasks"):
        checkpoint_ids.convert_config(config)
