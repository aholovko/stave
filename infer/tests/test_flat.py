"""Variant B behavior on a miniature backbone and toy-tokenizer projections."""

import copy
import dataclasses
import json
import math
import re
from pathlib import Path
from typing import Any

import pytest
import torch
from support import (
    P1_SRC,
    P3_SRC,
    SIX,
    TF_SRC,
    ToyTokenizer,
    expected_for,
    projections,
    selector,
    sha256,
    toy_backbone,
)

from stave_infer import flat as fm
from stave_infer import structural as sm
from stave_infer import structure as ps
from stave_infer import variants


@pytest.fixture(scope="module")
def items() -> dict[str, ps.Projection]:
    return {
        **projections("P1", P1_SRC),
        **projections("P3", P3_SRC),
        **projections("TF", TF_SRC),
    }


def _results(task: str, source: bytes, tmp_path: Path) -> list[dict[str, Any]]:
    command = [*selector(), "--ledger", str(tmp_path / "l.json")]
    if task == "TF":
        command += ["--path", "p/x_test.go", "--sha256", sha256(source)]
    return ps.run_selector(command, task, source, "x")


# Admission


def test_admission_covers_every_s_collatable_and_intact_incomplete_projection(
    items: dict[str, ps.Projection],
) -> None:
    admitted = [p for p in items.values() if p.status == "admitted"]
    assert {p.task for p in admitted} == {"P1", "P3", "TF"}
    for p in admitted:
        sm.collate([p], "S")
        assert fm.admit(p) is None, p.candidate
    incomplete = items["TF:x:0"]
    assert incomplete.status == "incomplete"
    assert incomplete.role_status["ARGS"] == "no_token"
    with pytest.raises(ps.StructureError, match="incomplete"):
        sm.collate([incomplete], "S")
    assert fm.admit(incomplete) is None
    assert fm.collate([incomplete]).candidates == (incomplete.candidate,)


def test_admission_rejects_unusable_padded_and_inconsistent_projections_with_reasons(
    tmp_path: Path, items: dict[str, ps.Projection]
) -> None:
    results = _results("P3", P3_SRC, tmp_path)
    tok = ToyTokenizer()
    good = items[results[0]["cand"]]
    malformed = ps.project(
        tok,
        P3_SRC,
        dict(results[0], ok=False, reason="selector failed"),
        max_tokens=1023,
    )
    unassembled = ps.project(tok, P3_SRC, results[0], max_tokens=5)
    assert good.facts is not None and good.mu is not None and good.masks is not None
    padded = dataclasses.replace(
        good,
        facts=good.facts + [list(ps.PAD) for _ in range(4)],
        mu=good.mu + [[0] * len(ps.ROLES[good.task]) for _ in range(4)],
        masks={
            arm: [row + [-math.inf] * 4 for row in mask]
            for arm, mask in good.masks.items()
        },
    )
    assert padded.status == "admitted" and len(padded.mu or []) == good.tokens + 4
    with pytest.raises(sm.ModelError, match="fact rows"):
        sm.collate([padded], "S")
    empty = dataclasses.replace(good, mu=[[0, *row[1:]] for row in good.mu])
    ragged = dataclasses.replace(good, mu=[row[:1] for row in good.mu])
    short = dataclasses.replace(good, mu=good.mu[:-1])
    cases = {
        "malformed": (malformed, "malformed: selector failed"),
        "unassembled": (unassembled, "unassembled"),
        "padded": (padded, "padded or inconsistent"),
        "short": (short, "padded or inconsistent"),
        "ragged": (ragged, "membership rows"),
        "empty CAND": (empty, "CAND has no token"),
        "no encoding": (dataclasses.replace(good, encoding=None), "no encoding"),
        "no mu": (dataclasses.replace(good, mu=None), "no encoding"),
    }
    for name, (p, reason) in cases.items():
        found = fm.admit(p)
        assert found is not None and reason in found, (name, found)
        with pytest.raises(sm.ModelError, match=f"{p.candidate}: .*{reason}"):
            fm.collate([p])


# Collation


def test_collate_carries_ids_cand_cue_and_source_pools_right_padded(
    items: dict[str, ps.Projection],
) -> None:
    chosen = [items[c] for c in ("P1:x:0", "P1:x:1", "P1:x:3")]
    batch = fm.collate(chosen)
    structural = sm.collate(chosen, "F")
    width = max(p.tokens for p in chosen)
    assert batch.task == "P1" and batch.ids.shape == (3, width)
    assert batch.candidates == tuple(p.candidate for p in chosen)
    for name in ("ids", "masks", "valid", "attention", "positions"):
        assert torch.equal(getattr(batch, name), getattr(structural, name)), name
    assert torch.equal(batch.cand, structural.mu[..., 0])
    assert batch.cand.dtype == torch.float32 and batch.masks.dtype == torch.float32
    for row, p in enumerate(chosen):
        assert p.mu is not None and p.encoding is not None
        assert batch.cand[row, : p.tokens].tolist() == [m[0] for m in p.mu]
        assert not batch.cand[row, p.tokens :].any()
        source = [t.end is not None for t in p.encoding.tokens]
        pools = batch.masks[row, :, : p.tokens].eq(0)
        assert all(pools[r].tolist() == source for r in range(len(ps.ROLES["P1"])))
        assert batch.masks[row, :, p.tokens :].eq(-math.inf).all()
    with pytest.raises(sm.ModelError, match="task"):
        fm.collate([items["P1:x:0"], items["P3:x:0"]])


def test_collate_ignores_facts_non_cand_membership_masks_and_role_status(
    items: dict[str, ps.Projection],
) -> None:
    p = items["P1:x:1"]
    assert p.facts is not None and p.mu is not None and p.masks is not None
    other = dataclasses.replace(
        p,
        facts=[
            [(v + 1) % b for v, b in zip(row, ps.BUCKETS, strict=True)]
            for row in p.facts
        ],
        mu=[[row[0], *(1 - v for v in row[1:])] for row in p.mu],
        masks={"F": p.masks["S"], "S": p.masks["F"]},
        role_status={},
    )
    assert fm.admit(other) is None
    first, second = fm.collate([p]), fm.collate([other])
    for field in dataclasses.fields(first):
        a, b = getattr(first, field.name), getattr(second, field.name)
        assert torch.equal(a, b) if isinstance(a, torch.Tensor) else a == b, field.name


# Shared initialization


def _new(model: Any) -> dict[str, Any]:
    return {n: p for n, p in model.named_parameters() if not n.startswith("backbone.")}


def test_construction_binds_untrained_structural_tensors() -> None:
    backbone = toy_backbone()
    before = {k: v.clone() for k, v in backbone.state_dict().items()}
    torch.manual_seed(11)
    rng = torch.random.get_rng_state()
    flat = fm.FlatModel(backbone, tasks=SIX, seed=11)
    assert torch.equal(torch.random.get_rng_state(), rng)
    assert all(torch.equal(before[k], v) for k, v in backbone.state_dict().items())
    assert flat.backbone is backbone and flat.tasks == SIX
    structural = _new(sm.StructuralModel(toy_backbone(), tasks=SIX, seed=11))
    new = _new(flat)
    heads = [n for n in structural if n.startswith("heads.")]
    assert list(new) == ["inject.weight", *(f"cand.{t}" for t in SIX), *heads]
    assert [n for n, _ in flat.added()] == list(new)
    assert torch.equal(new["inject.weight"], structural["inject.weight"])
    assert not new["inject.weight"].any()
    for t in SIX:
        row = structural[f"roles.{t}.weight"][:1]
        assert new[f"cand.{t}"].shape == (1, sm.FACT_WIDTH)
        assert torch.equal(new[f"cand.{t}"], row) and row.any()
    for name in heads:
        assert torch.equal(new[name], structural[name]), name
    assert flat.init == {
        "from": "stave-structural-6",
        "seed": 11,
        "tasks": list(SIX),
        "sha256": fm.digest(flat),
    }
    other = fm.FlatModel(toy_backbone(), tasks=SIX, seed=12)
    assert other.init["sha256"] != flat.init["sha256"]
    assert (
        fm.digest(fm.FlatModel(toy_backbone(), tasks=SIX, seed=11))
        == flat.init["sha256"]
    )
    for bad in (("P1", "P1"), ("X",), ()):
        with pytest.raises(sm.ModelError):
            fm.FlatModel(toy_backbone(), tasks=bad, seed=11)


# Input isolation and candidate cue


@pytest.fixture
def one_thread() -> Any:
    threads = torch.get_num_threads()
    torch.set_num_threads(1)
    yield
    torch.set_num_threads(threads)


@pytest.mark.usefixtures("one_thread")
def test_initial_logits_equal_the_untrained_structural_f_arm(
    items: dict[str, ps.Projection],
) -> None:
    backbone = toy_backbone()
    structural = sm.StructuralModel(backbone, tasks=SIX, seed=11).eval()
    flat = fm.FlatModel(backbone, tasks=SIX, seed=11).eval()
    for task in ("P1", "P3"):
        chosen = [p for p in items.values() if p.task == task][:4]
        assert len(chosen) >= 3, task
        with torch.no_grad():
            b = flat(fm.collate(chosen)).logits
            s = structural(sm.collate(chosen, "F")).logits
        assert torch.equal(b, s), task


def _nonzero_inject(*models: Any) -> None:
    generator = torch.Generator().manual_seed(5)
    weight = torch.randn(models[0].inject.weight.shape, generator=generator)
    with torch.no_grad():
        for model in models:
            model.inject.weight.copy_(weight)


def test_unused_facts_and_non_cand_membership_cannot_move_b(
    items: dict[str, ps.Projection],
) -> None:
    backbone = toy_backbone()
    structural = sm.StructuralModel(backbone, tasks=SIX, seed=11).eval()
    flat = fm.FlatModel(backbone, tasks=SIX, seed=11).eval()
    _nonzero_inject(structural, flat)
    chosen = [items["P1:x:1"], items["P1:x:2"]]
    changed = [
        dataclasses.replace(
            p,
            facts=[
                [(v + 3) % b for v, b in zip(row, ps.BUCKETS, strict=True)]
                for row in p.facts or []
            ],
            mu=[[row[0], *(1 - v for v in row[1:])] for row in p.mu or []],
        )
        for p in chosen
    ]
    assert all(fm.admit(p) is None for p in changed)
    with torch.no_grad():
        assert torch.equal(
            flat(fm.collate(chosen)).logits, flat(fm.collate(changed)).logits
        )
        s = structural(sm.collate(chosen, "F")).logits
        # The structural model must respond to show that B's unchanged output is meaningful.
        assert not torch.equal(s, structural(sm.collate(changed, "F")).logits)


def test_the_cand_cue_reaches_the_encoder(items: dict[str, ps.Projection]) -> None:
    flat = fm.FlatModel(toy_backbone(), tasks=SIX, seed=11).eval()
    p = items["P1:x:0"]
    assert p.mu is not None and p.encoding is not None
    target = next(
        i
        for i, (t, row) in enumerate(zip(p.encoding.tokens, p.mu, strict=True))
        if t.end is not None and not row[0]
    )
    mu = [[int(i == target), *row[1:]] for i, row in enumerate(p.mu)]
    moved = dataclasses.replace(p, candidate=p.candidate + "'", mu=mu)
    first, second = fm.collate([p]), fm.collate([moved])
    assert torch.equal(first.ids, second.ids) and torch.equal(first.masks, second.masks)
    with torch.no_grad():
        zero = [flat.encode(b).last_hidden_state for b in (first, second)]
        assert torch.equal(zero[0], zero[1])
        _nonzero_inject(flat)
        states = [flat.encode(b).last_hidden_state for b in (first, second)]
        assert not torch.equal(states[0], states[1])


def test_batched_logits_match_singletons_and_padding_is_never_pooled(
    items: dict[str, ps.Projection],
) -> None:
    flat = fm.FlatModel(toy_backbone(), tasks=SIX, seed=11).eval()
    _nonzero_inject(flat)
    chosen = [items["P1:x:0"], items["P1:x:1"], items["P1:x:3"]]
    batch = fm.collate(chosen)
    assert (~batch.valid).any()
    with torch.no_grad():
        out = flat(batch)
        for row, p in enumerate(chosen):
            alone = flat(fm.collate([p])).logits
            assert torch.allclose(out.logits[row : row + 1], alone, atol=1e-6)
        off = batch.masks[:, 0, :].isneginf()
        assert off.any() and off[~batch.valid].all()
        assert out.weights.masked_select(off[:, None, :]).eq(0).all()
        moved = dataclasses.replace(batch, ids=batch.ids.masked_fill(~batch.valid, 7))
        assert torch.equal(flat(moved).logits, out.logits)


# Save and strict reload


def _trained(model: Any) -> Any:
    generator = torch.Generator().manual_seed(5)
    with torch.no_grad():
        for p in model.parameters():
            p.add_(torch.randn(p.shape, generator=generator) * 0.05)
    return model


def test_save_and_reload_onto_a_fresh_backbone_are_bitwise(
    tmp_path: Path, items: dict[str, ps.Projection]
) -> None:
    flat = _trained(fm.FlatModel(toy_backbone(), tasks=SIX, seed=19)).eval()
    expected = expected_for(flat.backbone)
    batch = fm.collate([items["P3:x:1"], items["P3:x:2"]])
    with torch.no_grad():
        logits = flat(batch).logits
    fm.save(flat, tmp_path / "flat.pt", expected)

    saved = torch.load(tmp_path / "flat.pt", weights_only=True)
    config = json.loads(saved["config_json"])
    assert saved["format"] == config["format"] == "stave-flat-1"
    assert config["kind"] == "flat-cand" and config["tasks"] == list(SIX)
    structural = sm.StructuralModel(flat.backbone, tasks=SIX, seed=19)
    assert (
        config["backbone"]
        == json.loads(sm.config_json(structural, expected))["backbone"]
    )
    assert config["slots"] == {t: len(ps.ROLES[t]) for t in SIX}
    assert config["cue"] == fm.CUE and config["pool"] == fm.POOL
    assert config["init"] == flat.init and flat.init["sha256"] != fm.digest(flat)

    again = fm.load(tmp_path / "flat.pt", toy_backbone(initialize=False), expected)
    assert not again.training and fm.digest(again) == fm.digest(flat)
    assert again.seed == flat.seed == 19
    state = again.state_dict()
    assert list(state) == list(flat.state_dict())
    assert all(torch.equal(v, state[k]) for k, v in flat.state_dict().items())
    with torch.no_grad():
        assert torch.equal(again(batch).logits, logits)


def _changed(value: Any) -> list[Any]:
    """Return altered copies of one configuration value, one per nested leaf."""
    if isinstance(value, dict):
        return [
            {**value, key: other}
            for key, item in value.items()
            for other in _changed(item)
        ]
    if isinstance(value, bool):
        return [not value]
    if isinstance(value, int | float):
        return [value + 1]
    if isinstance(value, str):
        return [value + "x"]
    if isinstance(value, list):
        return [value[::-1] if len(value) > 1 else [*value, *value], value[:-1]]
    raise AssertionError(value)


def test_load_rejects_formats_config_fields_tensor_inventory_and_backbone(
    tmp_path: Path,
) -> None:
    backbone = toy_backbone()
    expected = expected_for(backbone)
    fm.save(fm.FlatModel(backbone, tasks=SIX, seed=11), tmp_path / "flat.pt", expected)
    sm.save(
        sm.StructuralModel(backbone, tasks=SIX, seed=11), tmp_path / "s.pt", expected
    )
    with pytest.raises(sm.ModelError, match="format"):
        fm.load(tmp_path / "s.pt", toy_backbone(), expected)
    with pytest.raises(sm.ModelError, match="format"):
        sm.load(tmp_path / "flat.pt", toy_backbone(), expected)
    assert fm.load(tmp_path / "flat.pt", toy_backbone(), expected).tasks == SIX

    saved = torch.load(tmp_path / "flat.pt", weights_only=True)
    config = json.loads(saved["config_json"])
    bad = tmp_path / "bad.pt"

    def rejected(variant: dict[str, Any], match: str | None = None) -> None:
        torch.save(variant, bad)
        with pytest.raises(sm.ModelError, match=match):
            fm.load(bad, toy_backbone(), expected)

    rejected({**saved, "format": "stave-flat-0"}, "format")
    rejected({**saved, "config_json": "{"}, "config")
    rejected({**saved, "state": []}, "state")
    altered = 0
    for key, value in config.items():
        for other in _changed(value):
            # These fields need separate cases: the digest is ignored and the seed rebuilds B.
            if key == "init" and any(other[k] != value[k] for k in ("sha256", "seed")):
                continue
            rejected({**saved, "config_json": json.dumps({**config, key: other})})
            altered += 1
    assert altered > len(config)
    for key, other in (("tasks", "P1"), ("init", {**config["init"], "seed": True})):
        rejected({**saved, "config_json": json.dumps({**config, key: other})}, "config")

    state = saved["state"]
    renamed = {("cand.X" if k == "cand.P1" else k): v for k, v in state.items()}
    missing = {k: v for k, v in state.items() if k != "heads.DC.empty"}
    for variant, names in (
        (renamed, "cand.P1, cand.X"),
        (missing, "heads.DC.empty"),
        ({**state, "extra": torch.zeros(1)}, "extra"),
        ({**state, "cand.P1": torch.zeros(2, sm.FACT_WIDTH)}, "cand.P1"),
        ({**state, "cand.P1": state["cand.P1"].double()}, "cand.P1"),
    ):
        rejected(
            {**saved, "state": copy.copy(variant)},
            rf"tensor.*differ: {re.escape(names)}$",
        )

    other = dataclasses.replace(expected, weight_sha256="2" * 64)
    with pytest.raises(sm.ModelError, match="config"):
        fm.load(tmp_path / "flat.pt", toy_backbone(), other)


def test_load_accepts_a_saved_init_digest_from_another_cpu_architecture(
    tmp_path: Path,
) -> None:
    flat = _trained(fm.FlatModel(toy_backbone(), tasks=SIX, seed=11)).eval()
    expected = expected_for(flat.backbone)
    fm.save(flat, tmp_path / "flat.pt", expected)
    saved = torch.load(tmp_path / "flat.pt", weights_only=True)
    config = json.loads(saved["config_json"])
    foreign = "a" * 64
    assert config["init"]["sha256"] != foreign
    config["init"]["sha256"] = foreign
    torch.save({**saved, "config_json": json.dumps(config)}, tmp_path / "x86.pt")

    again = fm.load(tmp_path / "x86.pt", toy_backbone(initialize=False), expected)
    assert again.saved_init_sha256 == foreign
    assert again.init["sha256"] == flat.init["sha256"]
    state = again.state_dict()
    assert list(state) == list(saved["state"])
    assert all(torch.equal(v, state[k]) for k, v in saved["state"].items())
    native = fm.load(tmp_path / "flat.pt", toy_backbone(), expected)
    assert native.saved_init_sha256 == flat.init["sha256"]


def test_load_still_refuses_any_other_config_difference_beside_the_digest(
    tmp_path: Path,
) -> None:
    backbone = toy_backbone()
    expected = expected_for(backbone)
    fm.save(fm.FlatModel(backbone, tasks=SIX, seed=11), tmp_path / "flat.pt", expected)
    saved = torch.load(tmp_path / "flat.pt", weights_only=True)
    config = json.loads(saved["config_json"])
    init = {**config["init"], "sha256": "a" * 64}
    bad = tmp_path / "bad.pt"
    for changed in (
        {"init": {**init, "from": "stave-structural-5"}},
        {"init": init, "slots": {**config["slots"], "P1": 9}},
        {"init": init, "cue": "other-cue"},
        {"init": init, "added_parameters": config["added_parameters"] + 1},
    ):
        torch.save({**saved, "config_json": json.dumps({**config, **changed})}, bad)
        with pytest.raises(
            sm.ModelError, match="saved config differs from the rebuilt"
        ):
            fm.load(bad, toy_backbone(), expected)
    # The loader rebuilds B from its saved seed; only the bundle can identify a foreign seed.
    reseeded = {**config, "init": {**init, "seed": 12}}
    torch.save({**saved, "config_json": json.dumps(reseeded)}, bad)
    assert fm.load(bad, toy_backbone(), expected).seed == 12
    with pytest.raises(variants.VariantError, match="saved seed 12 is not 11"):
        variants.load_model("B", bad, toy_backbone(), expected, SIX, 11)
