"""Parity comparisons on synthetic inputs.

These tests check which differences are accepted or reported without needing full model artifacts.
"""

import hashlib
import json
import sys
from pathlib import Path
from types import SimpleNamespace
from typing import Any

import pytest
import torch
from support import SIX, expected_for, tool, toy_backbone

from stave_infer import flat as fm
from stave_infer import bundle, serving

loader_parity = tool("parity/loader_parity.py")
case_study_parity = tool("parity/case_study_parity.py")


# Loader parity


def test_equal_state_dicts_are_equal_tensor_for_tensor() -> None:
    a = {"x": torch.arange(4.0), "y": torch.ones(2, 2)}
    b = {k: v.clone() for k, v in a.items()}
    assert loader_parity.compare_states(a, b) == {
        "tensors": 2, "equal": True, "differ": [], "missing": [], "extra": [],
    }  # fmt: skip


def test_a_one_ulp_difference_or_another_inventory_is_named() -> None:
    a = {"x": torch.tensor([1.0]), "y": torch.ones(2), "z": torch.zeros(1)}
    b = {
        "x": torch.nextafter(torch.tensor([1.0]), torch.tensor([2.0])),
        "y": torch.ones(2, dtype=torch.float64),
        "w": torch.zeros(1),
    }
    got = loader_parity.compare_states(a, b)
    assert got["equal"] is False
    assert got["differ"] == ["x", "y"]
    assert got["missing"] == ["w"] and got["extra"] == ["z"]


def _row(rid: str, span: list[int] | None, admission: str, bits: str | None) -> Any:
    logit = None if bits is None else 1.0
    entry = {"admission": admission, "reason": None, "logit": logit, "logit_f32": bits}
    return {"id": rid, "cand_span": span, "arms": {"S": entry}}


def _files(*rows: Any, task: str = "P1") -> list[dict[str, Any]]:
    return [{"task": task, "source": {"id": "f0"}, "rows": list(rows)}]


def test_judged_logits_compare_bit_for_bit() -> None:
    research = _files(
        _row("a", [1, 2], "admitted", "3f800000"),
        _row("b", None, "withheld", None),
        task="E1",
    )
    product = _files(
        _row("a", [1, 2], "admitted", "3f800000"), _row("b", None, "withheld", None)
    )
    assert loader_parity.compare_logits(research, product, "S") == {
        "judged": 1, "equal": True, "differ": [], "only_research": [], "only_product": [],
    }  # fmt: skip
    product[0]["rows"][0]["arms"]["S"]["logit_f32"] = "3f800001"
    product[0]["rows"][1] = _row("b", [3, 4], "admitted", "40000000")
    got = loader_parity.compare_logits(research, product, "S")
    assert got["equal"] is False
    assert got["differ"] == [["f0", "P1", "a", [1, 2], "3f800000", "3f800001"]]
    assert got["only_product"] == [["f0", "P1", "b", [3, 4]]]


@pytest.mark.parametrize(
    "research_errors,product_errors,changed_row,ok",
    [
        (0, 0, False, True),
        (1, 0, False, False),
        (0, 1, False, False),
        (1, 1, False, False),
        (0, 0, True, False),
    ],
)
def test_variant_success_requires_equal_reports_without_errors(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
    research_errors: int,
    product_errors: int,
    changed_row: bool,
    ok: bool,
) -> None:
    # Exercise the final decision while replacing artifact loading and serving with synthetic results.
    research = _files(task="E1")
    product = _files()
    if not (research_errors or product_errors):
        research[0]["rows"] = [_row("E1:f0:0", None, "withheld", None)]
        product[0]["rows"] = [_row("P1:f0:0", None, "withheld", None)]
    if changed_row:
        product[0]["rows"][0]["arms"]["S"]["reason"] = "changed withholding reason"
    args = SimpleNamespace(
        run=tmp_path / "run",
        model_dir=tmp_path,
        tokenizer_dir=tmp_path,
        tokenizer_receipt=tmp_path,
        research_selector=Path("research"),
        stave=Path("stave"),
    )
    model_path = args.run / "terminal" / "S" / "model.pt"
    model_path.parent.mkdir(parents=True)
    model_path.write_bytes(b"checkpoint")
    (args.run / "receipt.json").write_text('{"expected": {}}')
    bundle_dir = tmp_path / "bundle"
    bundle_dir.mkdir()
    (bundle_dir / "model.pt").write_bytes(model_path.read_bytes())
    sha = hashlib.sha256(model_path.read_bytes()).hexdigest()
    manifest = SimpleNamespace(
        id="toy-S",
        model=SimpleNamespace(file="model.pt", sha256=sha),
        provenance={"source_model_sha256": sha},
        tasks=["P1"],
        seed=11,
        thresholds={"P1": 0.0},
        batch_size=1,
    )
    model = torch.nn.Linear(1, 1)
    reference = SimpleNamespace(
        Source=serving.Source,
        load=lambda *a, **kw: SimpleNamespace(models={"S": model}),
        diagnose=lambda *a, **kw: {"files": research, "errors": research_errors},
    )
    monkeypatch.setitem(
        sys.modules,
        "stave",
        SimpleNamespace(
            checkpoint_smoke=SimpleNamespace(Expected=SimpleNamespace),
            pretrained_serving=reference,
        ),
    )
    monkeypatch.setattr(bundle, "read_manifest", lambda _: manifest)
    monkeypatch.setattr(serving, "load", lambda *a, **kw: SimpleNamespace(model=model))
    monkeypatch.setattr(
        serving,
        "diagnose",
        lambda *a, **kw: {
            "files": product,
            "errors": product_errors,
        },
    )
    result = loader_parity._variant("S", bundle_dir, args, tmp_path)
    assert result["logits"]["equal"] is True
    assert result["rows_equal"] is not changed_row
    assert result["errors"] == {"research": research_errors, "product": product_errors}
    assert result["ok"] is ok


def test_sources_follow_the_backend_task_split(tmp_path: Path) -> None:
    for name in ("a.go", "a_test.go"):
        (tmp_path / name).write_text("package a\n")
    (tmp_path / "notes.txt").write_text("")

    class Source:
        def __init__(self, task: str, path: Path, source_id: str, **extra: Any) -> None:
            self.fields = (task, path.name, source_id, extra)

    got = [s.fields for s in loader_parity.sources(Source, tmp_path)]
    a, t = tmp_path / "a.go", tmp_path / "a_test.go"
    assert got == [
        ("P1", "a.go", "f0", {"target": None, "tf_path": None}),
        ("P2", "a.go", "f0", {"target": a, "tf_path": None}),
        ("P3", "a.go", "f0", {"target": None, "tf_path": None}),
        ("DC", "a.go", "f0", {"target": None, "tf_path": None}),
        ("RP", "a.go", "f0", {"target": None, "tf_path": None}),
        ("TF", "a_test.go", "f1", {"target": None, "tf_path": str(t)}),
    ]


class _Checkpoint:
    """A toy implementation of the reference checkpoint loader."""

    def __init__(self) -> None:
        self.calls: list[tuple[Path, Any]] = []

    def load_checked(
        self, model_dir: Path, expected: Any
    ) -> tuple[Any, dict[str, Any]]:
        self.calls.append((model_dir, expected))
        return toy_backbone(), {}


def _foreign_b(tmp_path: Path, **state_change: Any) -> tuple[Path, Any, Any, str]:
    """Simulate a checkpoint whose initialization digest comes from a different CPU architecture."""
    flat = fm.FlatModel(toy_backbone(), tasks=SIX, seed=11)
    with torch.no_grad():
        for p in flat.parameters():
            p.add_(0.01)
    expected = expected_for(flat.backbone)
    fm.save(flat, tmp_path / "flat.pt", expected)
    saved = torch.load(tmp_path / "flat.pt", weights_only=True)
    config = json.loads(saved["config_json"])
    config["init"]["sha256"] = "a" * 64
    state = {**saved["state"], **state_change}
    torch.save(
        {**saved, "config_json": json.dumps(config), "state": state},
        tmp_path / "B.pt",
    )
    return tmp_path / "B.pt", expected, saved["state"], flat.init["sha256"]


def test_a_refused_b_is_rebuilt_from_the_research_classes_and_strict_loaded(
    tmp_path: Path,
) -> None:
    path, expected, state, rebuilt = _foreign_b(tmp_path)
    checkpoint = _Checkpoint()
    model, check = loader_parity.research_reference(
        "B", path, tmp_path / "pretrained", expected, SIX, 11,
        flat=fm, checkpoint=checkpoint,
    )  # fmt: skip
    assert checkpoint.calls == [(tmp_path / "pretrained", expected)]
    assert isinstance(model, fm.FlatModel) and not model.training
    got = model.state_dict()
    assert list(got) == list(state)
    assert all(torch.equal(v, got[k]) for k, v in state.items())
    assert check == {
        "refused": "saved config differs from the rebuilt model",
        "differs": {"init.sha256": {"saved": "a" * 64, "rebuilt": rebuilt}},
    }


def test_the_research_reference_still_refuses_seed_tasks_and_a_non_strict_state(
    tmp_path: Path,
) -> None:
    path, expected, _, _ = _foreign_b(tmp_path)

    def reference(path: Path, tasks: Any = SIX, seed: int = 11) -> Any:
        return loader_parity.research_reference(
            "B", path, tmp_path, expected, tasks, seed,
            flat=fm, checkpoint=_Checkpoint(),
        )  # fmt: skip

    with pytest.raises(ValueError, match="saved seed 11 is not 12"):
        reference(path, seed=12)
    with pytest.raises(ValueError, match="inventory"):
        reference(path, tasks=SIX[:5])
    with pytest.raises(ValueError, match="only B"):
        loader_parity.research_reference(
            "S", path, tmp_path, expected, SIX, 11, flat=fm, checkpoint=_Checkpoint()
        )
    extra, _, _, _ = _foreign_b(tmp_path, extra=torch.zeros(1))
    with pytest.raises(ValueError, match="tensor names"):
        reference(extra)


# Case-study parity

FROZEN = {"/w/r/a.go": ("r", "a.go"), "/w/r/a_test.go": ("r", "a_test.go")}


def _report(digest: str = "c1") -> dict[str, Any]:
    def diag(cls: str, origin: str, start: int, score: float | None) -> Any:
        return {
            "class": cls, "origin": origin, "source_sha256": "s1",
            "context_digest": digest, "span": {"start": start, "end": start + 4},
            "score": score,
        }  # fmt: skip

    return {
        "advisory": {"provider": "model", "status": "ok"},
        "files": [
            {
                "path": "/w/r/a.go",
                "role": "target",
                "diagnostics": [
                    diag("P1", "exact", 10, None),
                    diag("P1", "advisory", 10, 8.3),
                ],
                "coverage": [
                    {"class": "P1", "origin": "exact", "items": []},
                    {
                        "class": "P1",
                        "origin": "advisory",
                        "items": [{}],
                        "candidates": 3,
                    },
                    {"class": "P3", "origin": "advisory", "items": [], "candidates": 5},
                    {"class": "TF", "origin": "advisory", "items": [{}]},
                ],
            },
            {
                "path": "/w/r/a_test.go",
                "role": "test-target",
                "diagnostics": [diag("TF", "advisory", 40, 3.1)],
                "coverage": [
                    {
                        "class": "TF",
                        "origin": "advisory",
                        "items": [{}, {}],
                        "candidates": 4,
                    }
                ],
            },
            {"path": "/w/r/b.go", "role": "context", "diagnostics": [], "coverage": []},
        ],
    }


def _ledger() -> dict[str, Any]:
    def finding(cls: str, origin: str, path: str, start: int, score: Any) -> Any:
        return {
            "kind": "mapped", "class": cls, "origin": origin, "repository": "r",
            "relative_path": path,
            "data": {
                "class": cls, "origin": origin, "source_sha256": "s1", "context_digest": "c1",
                "span": {"start": start, "end": start + 4}, "score": score,
            },
        }  # fmt: skip

    return {
        "findings": [
            {"kind": "coverage", "class": None},
            finding("E1", "exact", "a.go", 10, None),
            finding("E1", "advisory", "a.go", 10, 8.3004),
            finding("TF", "advisory", "a_test.go", 40, 3.1),
        ]
    }


def test_code_diagnostics_compare_by_identity() -> None:
    got = case_study_parity.code_identities([_report()], FROZEN)
    want = case_study_parity.ledger_identities(_ledger())
    assert got == want and len(got) == 3
    assert ("P1", "advisory", "r", "a.go", "s1", 10, 14, "c1") in got
    assert case_study_parity.code_identities([_report("c2")], FROZEN) != want


def test_advisory_scores_compare_within_the_tolerance() -> None:
    got = case_study_parity.score_deltas([_report()], FROZEN, _ledger())
    assert got == pytest.approx(
        {("P1", "r", "a.go", 10, 14): 0.0004, ("TF", "r", "a_test.go", 40, 44): 0.0}
    )


def test_coverage_counts_per_class_skip_unchecked_classes() -> None:
    assert case_study_parity.coverage_counts([_report()], FROZEN) == {
        "P1": {"reported": 3, "judged": 2, "not_judged": 1, "advised": 1},
        "P3": {"reported": 5, "judged": 5, "not_judged": 0, "advised": 0},
        "TF": {"reported": 4, "judged": 2, "not_judged": 2, "advised": 1},
    }


def _serving(path: str, rows: list[Any], task: str = "P1") -> dict[str, Any]:
    return {
        "files": [
            {"task": task, "source": {"path": path, "sha256": "s1"}, "rows": rows}
        ]
    }


def _scored(span: list[int] | None, logit: float | None, admission: str) -> Any:
    return {"cand_span": span, "arms": {"S": {"admission": admission, "logit": logit}}}


def test_judged_logits_are_keyed_by_file_task_and_span() -> None:
    reports = [
        _serving(
            "/w/r/a.go",
            [_scored([1, 2], 0.5, "admitted"), _scored(None, None, "withheld")],
        ),
        _serving("/elsewhere/a.go", [_scored([1, 2], 9.0, "admitted")]),
    ]
    got = case_study_parity.judged_logits(reports, FROZEN, {"r/a.go": "s1"})
    assert got == {("r", "a.go", "P1", 1, 2): 0.5}
    with pytest.raises(ValueError, match="sha256"):
        case_study_parity.judged_logits(reports, FROZEN, {"r/a.go": "s2"})


def test_logits_compare_on_the_same_judged_set() -> None:
    retained = {("r", "a.go", "P1", 1, 2): 0.5, ("r", "a.go", "P3", 3, 4): -2.0}
    product = {("r", "a.go", "P1", 1, 2): 0.5004, ("r", "a.go", "P3", 3, 4): -2.0}
    got = case_study_parity.compare_judged(retained, product, 1e-3)
    assert got["judged"] == 2 and got["ok"] is True
    assert got["max_abs_delta"] == pytest.approx(0.0004)
    product[("r", "a.go", "P3", 3, 4)] = 0.1
    del product[("r", "a.go", "P1", 1, 2)]
    got = case_study_parity.compare_judged(retained, product, 1e-3)
    assert got["ok"] is False
    assert got["only_retained"] == [["r", "a.go", "P1", 1, 2]]
    assert got["beyond_tolerance"] == [["r", "a.go", "P3", 3, 4, -2.0, 0.1]]
    assert got["decisions_differ"] == [["r", "a.go", "P3", 3, 4, -2.0, 0.1]]


def test_two_reports_are_the_same_apart_from_the_provider() -> None:
    a, b = _report(), _report()
    b["advisory"]["provider"] = "command"
    assert case_study_parity.same_but_provider(a, b)
    b["files"][0]["diagnostics"][1]["score"] = 8.4
    assert not case_study_parity.same_but_provider(a, b)


def test_research_state_keys_map_to_product_names_without_changing_tensors() -> None:
    research = {
        "roles.E1.weight": torch.ones(2, 3),
        "heads.E2.values": torch.arange(4.0),
        "cand.E5": torch.zeros(1, 3),
    }
    product = {
        "roles.P1.weight": research["roles.E1.weight"].clone(),
        "heads.P2.values": research["heads.E2.values"].clone(),
        "cand.P3": research["cand.E5"].clone(),
    }
    assert loader_parity.compare_states(research, product)["equal"]
    product["heads.P2.values"][0] = 1
    assert loader_parity.compare_states(research, product)["differ"] == [
        "heads.P2.values"
    ]


def test_historical_candidate_prefixes_compare_to_product_ids() -> None:
    old = _files(_row("E1:f0:0", [1, 2], "admitted", "3f800000"), task="E1")
    new = _files(_row("P1:f0:0", [1, 2], "admitted", "3f800000"))
    assert loader_parity.compare_logits(old, new, "S")["equal"]
    assert loader_parity.product_records(old) == new
    assert old[0]["task"] == "E1"  # Historical evidence was not rewritten.


def test_research_sources_keep_the_original_task_ids(tmp_path: Path) -> None:
    (tmp_path / "a.go").write_text("package a\n")

    def source(task: str, path: Path, ident: str, **kwargs: Any) -> Any:
        return task, kwargs["target"]

    assert loader_parity.sources(source, tmp_path, research=True) == [
        ("E1", None),
        ("E2", tmp_path / "a.go"),
        ("E5", None),
        ("DC", None),
        ("RP", None),
    ]


def test_historical_case_study_tasks_are_normalized_on_read() -> None:
    reports = [_serving("/w/r/a.go", [_scored([1, 2], 0.5, "admitted")], task="E1")]
    got = case_study_parity.judged_logits(
        reports, FROZEN, {"r/a.go": "s1"}, historical=True
    )
    assert got == {("r", "a.go", "P1", 1, 2): 0.5}
    assert reports[0]["files"][0]["task"] == "E1"
