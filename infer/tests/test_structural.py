"""F/S model behavior on a miniature backbone and real selector projections."""

import dataclasses
import json
import math
import re
from pathlib import Path
from typing import Any

import pytest
import torch
from support import P1_SRC, P3_SRC, SIX, expected_for, projections, toy_backbone

from stave_infer import backbone as bb
from stave_infer import structural as sm
from stave_infer import structure as ps

SOURCE = ps.SEGMENTS.index("Source")
HEAD = (
    "values",
    "role_bias",
    "empty",
    "query.weight",
    "key.weight",
    "hidden.weight",
    "hidden.bias",
    "out.weight",
    "out.bias",
)


@pytest.fixture(scope="module")
def p1() -> dict[str, ps.Projection]:
    return projections("P1", P1_SRC)


@pytest.fixture(scope="module")
def p3() -> dict[str, ps.Projection]:
    return projections("P3", P3_SRC)


def _model(seed: int = 11) -> sm.StructuralModel:
    return sm.StructuralModel(toy_backbone(), tasks=SIX, seed=seed).eval()


def _new(model: Any) -> dict[str, Any]:
    return {n: p for n, p in model.named_parameters() if not n.startswith("backbone.")}


# Inventory and initialization


def test_added_parameters_have_the_frozen_names_and_initial_tensors() -> None:
    backbone = toy_backbone()
    before = {k: v.clone() for k, v in backbone.state_dict().items()}
    torch.manual_seed(3)
    rng = torch.random.get_rng_state()
    model = sm.StructuralModel(backbone, tasks=SIX, seed=11)
    assert torch.equal(torch.random.get_rng_state(), rng)
    new = _new(model)
    assert list(new) == [
        *(f"facts.{n}.weight" for n in sm.FACT_NAMES),
        *(f"roles.{t}.weight" for t in SIX),
        "inject.weight",
        *(f"heads.{t}.{n}" for t in SIX for n in HEAD),
    ]
    assert [new[f"facts.{n}.weight"].shape[0] for n in sm.FACT_NAMES] == list(
        ps.BUCKETS
    )
    assert not new["inject.weight"].any()
    for name, p in new.items():
        if name.endswith("bias"):
            assert not p.any(), name
        elif name != "inject.weight":
            assert abs(float(p.detach().std()) - 0.02) < 0.01, name
    assert all(torch.equal(before[k], v) for k, v in backbone.state_dict().items())
    again = _new(sm.StructuralModel(toy_backbone(), tasks=SIX, seed=11))
    assert all(torch.equal(p, again[n]) for n, p in new.items())
    other = _new(sm.StructuralModel(toy_backbone(), tasks=SIX, seed=12))
    assert not torch.equal(other["heads.P1.values"], new["heads.P1.values"])


def test_task_inventories_are_canonical_subsequences_of_the_six() -> None:
    assert sm.TASKS == SIX
    for tasks in (SIX, ("P1",), ("P2", "TF"), ("TF", "RP")):
        assert sm.check_tasks(tasks) == tasks
    assert sm.format_of(SIX) == sm.format_of(("DC",)) == "stave-structural-6"
    assert sm.format_of(("P1", "P2", "P3", "TF")) == "stave-structural-4"
    assert sm.format_of(("P1", "P3")) == "stave-structural-3"
    assert sm.format_of(("P1",)) == "stave-structural-2"
    for bad in [(), ("TF", "P1"), ("P1", "P1"), ("DS",), ("A1", "P1")]:
        with pytest.raises(sm.ModelError):
            sm.check_tasks(bad)


def test_init_order_appends_each_later_task_as_one_block() -> None:
    model = _model()
    order = sm.init_order(model)
    assert sorted(order) == sorted(n for n, _ in model.added())
    head = order[: order.index("heads.P1.out.bias") + 1]
    assert head[:10] == [
        *(f"facts.{n}.weight" for n in sm.FACT_NAMES),
        "roles.P1.weight",
        "inject.weight",
    ]
    rest = order[len(head) :]
    for i, task in enumerate(SIX[1:]):
        block = rest[i * 10 : (i + 1) * 10]
        assert block == [f"roles.{task}.weight", *(f"heads.{task}.{n}" for n in HEAD)]


# Collation


@pytest.mark.parametrize("arm", ("F", "S"))
def test_collate_right_pads_facts_mu_and_masks(
    p1: dict[str, ps.Projection], arm: str
) -> None:
    items = list(p1.values())[:3]
    batch = sm.collate(items, arm)
    width = max(p.tokens for p in items)
    assert any(p.tokens < width for p in items)
    assert batch.ids.shape == (3, width) and batch.task == "P1"
    for row, p in enumerate(items):
        assert p.encoding is not None
        pad = slice(p.tokens, width)
        assert batch.ids[row, pad].eq(bb.PAD_ID).all()
        assert batch.facts[row, pad].tolist() == [list(ps.PAD)] * (width - p.tokens)
        assert not batch.mu[row, pad].any()
        assert batch.masks[row, :, pad].eq(-math.inf).all()
        assert batch.facts[row, : p.tokens].tolist() == p.facts
        assert batch.ids[row, : p.tokens].tolist() == [t.id for t in p.encoding.tokens]
    assert torch.equal(
        batch.attention, batch.valid[:, None, :] & batch.valid[:, :, None]
    )


def test_collate_rejects_unadmitted_projections_and_mixed_tasks(
    p1: dict[str, ps.Projection], p3: dict[str, ps.Projection]
) -> None:
    admitted = p1["P1:x:0"]
    for status in ("incomplete", "malformed", "unassembled"):
        withheld = dataclasses.replace(admitted, status=status)
        with pytest.raises(ps.StructureError, match=status):
            sm.collate([withheld], "S")
    with pytest.raises(sm.ModelError, match="task"):
        sm.collate([admitted, p3["P3:x:0"]], "F")


# Inference


def test_all_source_s_masks_reproduce_f_logits(p3: dict[str, ps.Projection]) -> None:
    model = _model()
    items = [p3["P3:x:0"], p3["P3:x:1"]]
    f, s = sm.collate(items, "F"), sm.collate(items, "S")
    assert not torch.equal(f.masks, s.masks)
    with torch.no_grad():
        f_logits = model(f).logits
        substituted = model(dataclasses.replace(s, masks=f.masks)).logits
        s_logits = model(s).logits
    assert torch.equal(f_logits, substituted)
    assert not torch.equal(f_logits, s_logits)


def test_head_weights_are_zero_off_source_and_padding_cannot_move_logits(
    p1: dict[str, ps.Projection],
) -> None:
    model = _model()
    items = [p1["P1:x:0"], p1["P1:x:1"], p1["P1:x:3"]]
    for arm in ("F", "S"):
        batch = sm.collate(items, arm)
        off = batch.facts[..., 7].ne(SOURCE)
        assert off.any() and (~batch.valid).any()
        with torch.no_grad():
            out = model(batch)
            assert out.weights.masked_select(off[:, None, :]).eq(0).all()
            moved = dataclasses.replace(
                batch, ids=batch.ids.masked_fill(~batch.valid, 7)
            )
            assert torch.equal(model(moved).logits, out.logits)
            hidden = model.encode(batch).last_hidden_state
            noisy = hidden.masked_fill(~batch.valid[..., None], 1e3)
            head = model.heads["P1"]
            assert torch.equal(
                head(noisy, batch.mu, batch.masks).logits,
                head(hidden, batch.mu, batch.masks).logits,
            )


def test_empty_s_role_uses_its_learned_vector_and_f_never_does(
    p1: dict[str, ps.Projection],
) -> None:
    empty = p1["P1:x:3"]
    j = ps.ROLES["P1"].index("THEN_LAST")
    assert empty.role_status["THEN_LAST"] == "structural_empty"
    model = _model()
    head = model.heads["P1"]
    assert isinstance(head, sm.RoleHead)
    items = [empty, p1["P1:x:0"]]
    with torch.no_grad():
        s = sm.collate(items, "S")
        assert s.masks[0, j].eq(-math.inf).all()
        out = model(s)
        assert torch.isfinite(out.logits).all() and torch.isfinite(out.pooled).all()
        assert torch.equal(out.pooled[0, j], head.empty[j])
        f = model(sm.collate(items, "F"))
        assert not torch.equal(f.pooled[0, j], head.empty[j])


# Save and strict reload


def _trained(model: sm.StructuralModel) -> sm.StructuralModel:
    """Perturb every tensor so a reload cannot pass by retaining initialized weights."""
    generator = torch.Generator().manual_seed(5)
    with torch.no_grad():
        for p in model.parameters():
            p.add_(torch.randn(p.shape, generator=generator) * 0.05)
    return model


def test_save_and_reload_onto_a_fresh_backbone_keep_every_tensor_and_logits(
    tmp_path: Path, p1: dict[str, ps.Projection], p3: dict[str, ps.Projection]
) -> None:
    model = _trained(_model())
    expected = expected_for(model.backbone)
    batches = {
        "P1": sm.collate([p1["P1:x:1"], p1["P1:x:2"]], "S"),
        "P3": sm.collate([p3["P3:x:1"], p3["P3:x:4"]], "F"),
    }
    with torch.no_grad():
        logits = {t: model(b).logits for t, b in batches.items()}
    sm.save(model, tmp_path / "model.pt", expected)
    again = sm.load(tmp_path / "model.pt", toy_backbone(initialize=False), expected)
    state = again.state_dict()
    assert list(state) == list(model.state_dict())
    assert all(torch.equal(v, state[k]) for k, v in model.state_dict().items())
    assert not again.training and again.seed == 11 and again.tasks == SIX
    with torch.no_grad():
        for task, batch in batches.items():
            assert torch.equal(again(batch).logits, logits[task])
    saved = torch.load(tmp_path / "model.pt", weights_only=True)
    assert saved["format"] == "stave-structural-6"
    config = json.loads(saved["config_json"])
    assert config["tasks"] == list(SIX) and config["seed"] == 11
    assert config["backbone"]["revision"] == expected.revision
    assert config["init_order"] == sm.init_order(model)


def _resaved(tmp_path: Path, saved: dict[str, Any], **changes: Any) -> Path:
    path = tmp_path / "bad.pt"
    torch.save({**saved, **changes}, path)
    return path


def test_load_refuses_format_config_seed_and_pairing_violations(
    tmp_path: Path,
) -> None:
    model = _model()
    expected = expected_for(model.backbone)
    sm.save(model, tmp_path / "model.pt", expected)
    saved = torch.load(tmp_path / "model.pt", weights_only=True)
    config = json.loads(saved["config_json"])

    def refused(match: str, **changes: Any) -> None:
        with pytest.raises(sm.ModelError, match=match):
            sm.load(_resaved(tmp_path, saved, **changes), toy_backbone(), expected)

    refused("format", format="stave-structural-1")
    refused("format", format="other")
    refused("does not pair", format="stave-structural-4")
    altered = dict(config, added_parameters=config["added_parameters"] + 1)
    refused("config", config_json=json.dumps(altered, sort_keys=True))
    for seed in (None, True, 11.0, "11"):
        changed = dict(config)
        if seed is None:
            del changed["seed"]
        else:
            changed["seed"] = seed
        refused("seed", config_json=json.dumps(changed, sort_keys=True))
    refused("task inventory", config_json=json.dumps(dict(config, tasks="P1")))
    other = dataclasses.replace(expected, weight_sha256="2" * 64)
    with pytest.raises(sm.ModelError, match="config"):
        sm.load(tmp_path / "model.pt", toy_backbone(), other)
    wider = toy_backbone(hidden_size=32, num_attention_heads=2)
    with pytest.raises(sm.ModelError, match="config"):
        sm.load(tmp_path / "model.pt", wider, expected)


def test_load_refuses_a_saved_tensor_of_another_dtype(tmp_path: Path) -> None:
    # PyTorch's strict load would silently cast this dtype mismatch.
    model = _model()
    expected = expected_for(model.backbone)
    sm.save(model, tmp_path / "model.pt", expected)
    saved = torch.load(tmp_path / "model.pt", weights_only=True)
    state = dict(saved["state"])
    name = next(iter(state))
    state[name] = state[name].to(torch.int64)
    with pytest.raises(
        sm.ModelError, match=rf"dtypes or shapes differ: {re.escape(name)}$"
    ):
        sm.load(_resaved(tmp_path, saved, state=state), toy_backbone(), expected)
