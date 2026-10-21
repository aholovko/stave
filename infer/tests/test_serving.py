"""All three variants through the real selector, ledgers and tokenizer.

Toy models keep the tests small. Boundary cases alter selector output or model values to exercise refusals.
"""

import dataclasses
import hashlib
import math
import struct
from pathlib import Path
from types import SimpleNamespace
from typing import Any

import pytest
import torch
from support import TOKENIZER_DIR, selector

from stave_infer import bundle, serving, toy, variants
from stave_infer import structure as ps

ARMS = ("B", "F", "S")

P1 = (
    b"package p\r\n\r\n// F returns \xc3\xa9.\r\nfunc F(x int) (int, error) {\r\n"
    b"\tif x > 0 {\r\n\t\treturn x, nil\r\n\t} else if x < -1 {\r\n"
    b"\t\treturn -x, nil\r\n\t}\r\n\treturn 0, nil\r\n}\r\n\r\nfunc g() {}\r\n"
)
P2 = (
    b'package p\n\nimport "errors"\n\nvar msg = "m"\n'
    b'var _ = errors.New("Bad.")\nvar _ = (errors).New("x")\nvar _ = errors.New(msg)\n'
)
P3 = b"package p\r\n\r\nfunc F_a(p_\xcf\x80, _ int) {}\r\n"
LONG = (
    b"package p\n\nfunc H(v_x struct{"
    + b"".join(b" a%d int;" % i for i in range(400))
    + b" }) {}\n"
)
TF = (
    b'package p\r\n\r\nimport "testing"\r\n\r\n// TestP doc \xc3\xa9.\r\n'
    b"func TestP(t *testing.T) {\r\n"
    b'\tt.Error("")\r\n'
    b"\tfor _, c := range []int{1} {\r\n"
    b'\t\tt.Run("sub", func(u *testing.T) {\r\n'
    b"\t\t\tif c > 0 {\r\n"
    b'\t\t\t\tu.Errorf("got %d", c)\r\n'
    b"\t\t\t}\r\n"
    b'\t\t\tt.Fatal("outer", c)\r\n'
    b"\t\t})\r\n"
    b"\t}\r\n"
    b"}\r\n"
)
TF_PATH = "pkg/sub/p_test.go"
DC_RP = (
    b"package p\n\n// T is documented.\ntype T struct{}\n\n//go:noinline\nfunc (T) M() {}\n\n"
    b"// Pair returns two.\nfunc Pair() (int, int) { return 1, 2 }\n"
)
# One TF candidate between the 512 and 1023 framed-token budgets.
TF_LONG = (
    b'package p\n\nimport "testing"\n\nfunc TestP(t *testing.T) {\n\tt.Error("x"'
    + b", 1" * 300
    + b")\n}\n"
)


@pytest.fixture(scope="module")
def bundles(tmp_path_factory: pytest.TempPathFactory) -> dict[str, Path]:
    root = tmp_path_factory.mktemp("bundles")
    for arm in ARMS:
        toy.make_toy_bundle(arm, root / arm, tokenizer_dir=TOKENIZER_DIR)
    return {arm: root / arm for arm in ARMS}


@pytest.fixture(scope="module")
def loaded(bundles: dict[str, Path]) -> dict[str, serving.Loaded]:
    return {
        arm: serving.load(path, bundle.read_manifest(path), threads=None)
        for arm, path in bundles.items()
    }


def _write(directory: Path, name: str, data: bytes) -> Path:
    path = directory / name
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_bytes(data)
    return path


def _serve(loaded: serving.Loaded, sources: list[serving.Source]) -> dict[str, Any]:
    return serving.diagnose(loaded, sources, selector=selector())


def _spans(data: bytes, row: dict[str, Any], key: str = "cand_span") -> bytes:
    start, end = row[key]
    return data[start:end]


def _withheld(row: dict[str, Any], arm: str, prefix: str) -> None:
    entry = row["arms"][arm]
    assert entry["admission"] == "withheld" and entry["reason"].startswith(prefix)
    assert (entry["logit"], entry["decision"]) == (None, None)


def _altered(monkeypatch: pytest.MonkeyPatch, change: Any) -> list[int]:
    """Alter real selector output to test the serving boundary."""
    real, calls = ps.run_selector, [0]

    def run(*args: Any, **kwargs: Any) -> list[dict[str, Any]]:
        results = real(*args, **kwargs)
        calls[0] += 1
        change(results, calls[0])
        return results

    monkeypatch.setattr(ps, "run_selector", run)
    return calls


# Loading


def test_load_binds_the_bundle_configuration_and_identity(
    bundles: dict[str, Path], loaded: dict[str, serving.Loaded]
) -> None:
    for arm in ARMS:
        m, got = bundle.read_manifest(bundles[arm]), loaded[arm]
        assert got.arm == arm and got.tasks == bundle.TASKS and got.seed == m.seed
        assert got.thresholds == m.thresholds and got.batch_size == 2
        assert got.budgets == m.input_budgets
        assert got.identity["bundle"] == {"id": f"toy-{arm}", "variant": arm}
        assert got.identity["model"]["sha256"] == m.model.sha256
        assert not got.model.training


def test_load_refuses_a_tampered_model_before_building(
    tmp_path: Path, bundles: dict[str, Path]
) -> None:
    m = bundle.read_manifest(bundles["S"])
    copy = tmp_path / "S"
    toy.make_toy_bundle("S", copy, tokenizer_dir=TOKENIZER_DIR)
    with (copy / "model.pt").open("r+b") as f:
        f.seek(100)
        byte = f.read(1)
        f.seek(100)
        f.write(bytes([byte[0] ^ 1]))
    with pytest.raises(serving.ServingError, match="model.sha256"):
        serving.load(copy, m, threads=None)


def test_load_applies_the_thread_count_when_one_is_given(
    bundles: dict[str, Path],
) -> None:
    previous = torch.get_num_threads()
    threads = 3 if previous != 3 else 2
    try:
        serving.load(bundles["B"], bundle.read_manifest(bundles["B"]), threads=threads)
        assert torch.get_num_threads() == threads
    finally:
        torch.set_num_threads(previous)


# Selector and model integration


def test_serving_contract_selector_only_rows_keep_original_spans_and_model_logits(
    tmp_path: Path, loaded: dict[str, serving.Loaded]
) -> None:
    p1 = _write(tmp_path, "f.go", P1)
    dc_rp = _write(tmp_path, "d.go", DC_RP)
    sources = [
        serving.Source("P1", p1, "f"),
        serving.Source("DC", dc_rp, "d"),
        serving.Source("RP", dc_rp, "d"),
    ]
    for arm in ARMS:
        report = _serve(loaded[arm], sources)
        assert report["schema"] == serving.SCHEMA and report["errors"] == 0
        model = loaded[arm].model
        for source, file in zip(sources, report["files"], strict=True):
            data = source.path.read_bytes()
            assert file["task"] == source.task and file["outcome"] == "ok"
            assert file["source"] == {
                "id": source.source_id,
                "path": str(source.path),
                "bytes": len(data),
                "sha256": hashlib.sha256(data).hexdigest(),
            }
            direct = ps.run_selector(selector(), source.task, data, source.source_id)
            assert direct and [r["id"] for r in file["rows"]] == [
                r["cand"] for r in direct
            ]
            for row, result in zip(file["rows"], direct, strict=True):
                assert row["cand_span"] == result["roles"]["CAND"]["iv"][0]
                assert (row["anchor"], row["ledger_status"]) == (None, None)
                p = ps.project(loaded[arm].tokenizer, data, result, max_tokens=1023)
                entry, reason = row["arms"][arm], variants.admit(arm, p)
                if reason is not None:
                    assert entry["admission"] == "withheld"
                    continue
                with torch.no_grad():
                    want = model(variants.collate(arm, [p])).logits[0].item()
                assert entry["admission"] == "admitted" and entry["reason"] is None
                assert math.isclose(entry["logit"], want, rel_tol=1e-5, abs_tol=1e-6)
                assert entry["decision"] is (entry["logit"] >= 0.0)
                bits = struct.pack(">f", entry["logit"]).hex()
                assert entry["logit_f32"] == bits
        p1_file, dc, rp = report["files"]
        assert p1_file["coverage"] == {"enumeration": serving.SELECTOR_ONLY_ENUMERATION}
        assert dc["coverage"]["withheld"] == {"no_prose": 1}
        assert rp["coverage"]["withheld"] == {}
        assert [_spans(DC_RP, r) for r in dc["rows"]] == [b"// T is documented."]
        assert [_spans(DC_RP, r) for r in rp["rows"]] == [b"(int, int)"]


def test_serving_contract_p2_context_comes_only_from_the_caller_target(
    tmp_path: Path, loaded: dict[str, serving.Loaded]
) -> None:
    served = _write(tmp_path, "src/t.go", P2)
    package = _write(tmp_path, "pkg/t.go", P2)
    other = _write(tmp_path, "other/t.go", P2 + b"// changed\n")
    report = _serve(
        loaded["S"],
        [
            serving.Source("P2", served, "c", target=package),
            serving.Source("P2", served, "u"),
            serving.Source("P2", served, "m", target=other),
        ],
    )
    certified, unknown, mismatch = report["files"]
    assert [r["ledger_status"] for r in certified["rows"]] == [
        "eligible",
        "excluded_paren_base",
        "indirect_argument",
    ]
    assert certified["coverage"]["certificate"] is not None
    eligible, paren, indirect = certified["rows"]
    assert _spans(P2, eligible) == b'"Bad."' and eligible["eligible"] is True
    assert eligible["arms"]["S"]["admission"] == "admitted"
    assert isinstance(eligible["arms"]["S"]["decision"], bool)
    assert paren["cand_span"] is None and _spans(P2, paren, "anchor") == b'"x"'
    assert indirect["cand_span"] is None
    assert indirect["anchor"] == [P2.rindex(b"msg"), P2.rindex(b"msg") + 3]
    for row in (paren, indirect):
        assert row["eligible"] is False
        _withheld(row, "S", "not streamed")

    assert unknown["coverage"]["certificate"] is None
    context = unknown["rows"][0]
    assert context["ledger_status"] == "unknown_context"
    assert context["cand_span"] == eligible["cand_span"] and not context["eligible"]
    _withheld(context, "S", "ineligible: unknown_context")

    assert mismatch["outcome"] == "error" and mismatch["rows"] == []
    assert "differ" in mismatch["error"]
    assert report["errors"] == 1


def test_serving_contract_p3_ledger_only_and_overlength_rows_stay_visible(
    tmp_path: Path, loaded: dict[str, serving.Loaded]
) -> None:
    short = _write(tmp_path, "e.go", P3)
    long = _write(tmp_path, "long.go", LONG)
    report = _serve(
        loaded["B"],
        [serving.Source("P3", short, "e"), serving.Source("P3", long, "long")],
    )
    rows = report["files"][0]["rows"]
    assert [r["id"] for r in rows] == ["P3:e:0", "P3:e:1", "P3:e:2"]
    assert [r["ledger_status"] for r in rows] == [
        "candidate",
        "candidate",
        "excluded_blank",
    ]
    assert [_spans(P3, r) for r in rows[:2]] == [b"F_a", b"p_\xcf\x80"]
    for row in rows[:2]:
        assert row["arms"]["B"]["admission"] == "admitted"
        assert isinstance(row["arms"]["B"]["decision"], bool)
    blank = rows[2]
    assert blank["cand_span"] is None and _spans(P3, blank, "anchor") == b"_"
    _withheld(blank, "B", "not streamed: excluded_blank")

    over = [r for r in report["files"][1]["rows"] if r["cand_span"] is not None]
    v_x = next(r for r in over if _spans(LONG, r) == b"v_x")
    assert v_x["eligible"] is True and v_x["projection"] == "unassembled"
    _withheld(v_x, "B", "unassembled")
    counts = report["denominators"]["B"]["P3"]
    assert counts["rows"] == len(rows) + len(report["files"][1]["rows"])
    assert counts["withheld"]["unassembled"] >= 1
    assert counts["withheld"]["not streamed"] == 1


def test_serving_contract_tf_ledger_sentry_gate_and_b_only_admission(
    tmp_path: Path, loaded: dict[str, serving.Loaded]
) -> None:
    path = _write(tmp_path, "p_test.go", TF)
    sources = [
        serving.Source("TF", path, "p", tf_path=TF_PATH),
        serving.Source("TF", path, "v", tf_path="vendor/x/p_test.go"),
    ]
    reports = {arm: _serve(loaded[arm], sources) for arm in ARMS}
    for arm, report in reports.items():
        served, gated = report["files"]
        coverage = served["coverage"]
        assert coverage["path"] == TF_PATH
        assert coverage["path_gate"] == {"status": "eligible"}
        assert coverage["file"]["admitted"] == 3
        assert coverage["sentry"]["total"] == coverage["sentry"]["matched"] == 3
        admitted = [r for r in served["rows"] if r["id"] is not None]
        assert [r["id"] for r in admitted] == ["TF:p:0", "TF:p:1", "TF:p:2"]
        assert _spans(TF, admitted[0]) == b't.Error("")'
        assert all(r["context"] == "derived" and r["eligible"] for r in admitted)
        first = admitted[0]["arms"][arm]
        if arm == "B":
            assert first["admission"] == "admitted"
            assert isinstance(first["decision"], bool)
        else:
            assert first["reason"].startswith("incomplete")
            assert first["decision"] is None
        examined = [r for r in served["rows"] if r["id"] is None]
        assert examined, "every examined ledger row stays visible"
        for row in examined:
            assert row["cand_span"] is None and row["anchor"] is not None
            _withheld(row, arm, f"not streamed: {row['ledger_status']}")
        assert gated["outcome"] == "ok" and gated["rows"] == []
        assert gated["coverage"]["path_gate"] == {
            "status": "excluded",
            "reason": "vendor",
        }
        assert gated["coverage"]["file"] is None
        assert gated["coverage"]["sentry"]["unresolved"] == 3
    assert reports["B"]["denominators"]["B"]["TF"]["admitted"] == 3
    for arm in ("F", "S"):
        counts = reports[arm]["denominators"][arm]["TF"]
        assert counts["admitted"] == 2 and counts["withheld"]["incomplete"] == 1


def test_serving_contract_bundle_budgets_withhold_tf_past_512_tokens(
    tmp_path: Path, loaded: dict[str, serving.Loaded]
) -> None:
    path = _write(tmp_path, "p_test.go", TF_LONG)
    report = _serve(loaded["S"], [serving.Source("TF", path, "p", tf_path=TF_PATH)])
    (row,) = [r for r in report["files"][0]["rows"] if r["id"] is not None]
    reason = row["arms"]["S"]["reason"]
    assert row["projection"] == "unassembled" and reason.endswith("exceed 512")
    assert 512 < int(reason.split()[1]) <= 1023, "the 1023 budget would admit it"
    assert report["max_tokens"] == {
        "P1": 1023, "P2": 1023, "P3": 1023, "TF": 512, "DC": 512, "RP": 512,
    }  # fmt: skip
    wider = dataclasses.replace(
        loaded["S"], budgets={**loaded["S"].budgets, "TF": 1023}
    )
    report = _serve(wider, [serving.Source("TF", path, "p", tf_path=TF_PATH)])
    (row,) = [r for r in report["files"][0]["rows"] if r["id"] is not None]
    assert row["projection"] == "admitted"


# Invalid selector output, thresholds and models


def test_serving_contract_malformed_result_keeps_its_ledger_row(
    tmp_path: Path,
    loaded: dict[str, serving.Loaded],
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    def malformed(results: list[dict[str, Any]], _: int) -> None:
        first = results[0]
        for key in ("runs", "facts", "roles"):
            del first[key]
        first.update(ok=False, reason="authored selector failure")

    _altered(monkeypatch, malformed)
    path = _write(tmp_path, "e.go", P3)
    file = _serve(loaded["F"], [serving.Source("P3", path, "e")])["files"][0]
    assert file["outcome"] == "ok"
    row = file["rows"][0]
    assert row["id"] == "P3:e:0" and _spans(P3, row) == b"F_a"
    assert row["projection"] == "malformed"
    _withheld(row, "F", "malformed: authored selector failure")


@pytest.mark.parametrize(
    ("name", "change", "message"),
    [
        ("cand", lambda r: r[0]["roles"]["CAND"].update(iv=[[0, 1]]), "CAND"),
        ("missing", lambda r: r.pop(), "results"),
        ("id", lambda r: r[0].update(cand="P3:e:9"), "P3:e:9"),
        (
            "failed-source",
            lambda r: r[0].update(
                ok=False, reason="x", source={**r[0]["source"], "sha256": "0" * 64}
            ),
            "source",
        ),
    ],
)
def test_serving_contract_selector_join_failures_are_file_errors(
    tmp_path: Path,
    loaded: dict[str, serving.Loaded],
    monkeypatch: pytest.MonkeyPatch,
    name: str,
    change: Any,
    message: str,
) -> None:
    _altered(monkeypatch, lambda results, _: change(results))
    path = _write(tmp_path, "e.go", P3)
    report = _serve(loaded["S"], [serving.Source("P3", path, "e")])
    file = report["files"][0]
    assert report["errors"] == 1 and file["outcome"] == "error", name
    assert file["rows"] == [] and message in file["error"]


def test_serving_contract_an_unreadable_source_is_a_file_error(
    tmp_path: Path, loaded: dict[str, serving.Loaded]
) -> None:
    report = _serve(loaded["S"], [serving.Source("P3", tmp_path / "absent.go", "a")])
    file = report["files"][0]
    assert file["outcome"] == "error" and "absent.go" in file["error"]


def test_serving_contract_threshold_equal_to_the_logit_emits(
    tmp_path: Path, loaded: dict[str, serving.Loaded]
) -> None:
    path = _write(tmp_path, "e.go", P3)
    sources = [serving.Source("P3", path, "e")]

    def decision(threshold: float) -> Any:
        at = dataclasses.replace(
            loaded["B"], thresholds={**loaded["B"].thresholds, "P3": threshold}
        )
        return _serve(at, sources)["files"][0]["rows"][0]["arms"]["B"]

    logit = decision(0.0)["logit"]
    assert decision(logit)["decision"] is True
    assert decision(math.nextafter(logit, math.inf))["decision"] is False


def test_serving_contract_non_finite_logit_publishes_nothing(
    tmp_path: Path, loaded: dict[str, serving.Loaded]
) -> None:
    def nan(batch: Any) -> Any:
        return SimpleNamespace(logits=torch.full((batch.ids.shape[0],), math.nan))

    broken = dataclasses.replace(loaded["S"], model=nan)
    path = _write(tmp_path, "e.go", P3)
    with pytest.raises(serving.ServingError, match="non-finite"):
        _serve(broken, [serving.Source("P3", path, "e")])


@pytest.mark.parametrize(
    ("source", "message"),
    [
        (serving.Source("DS", Path("x.go"), "x"), "inventory"),
        (serving.Source("P3", Path("x.go"), "x", target=Path("y.go")), "only P2"),
        (serving.Source("TF", Path("x_test.go"), "x"), "tf_path"),
        (serving.Source("P3", Path("x.go"), "x", tf_path="x.go"), "tf_path"),
    ],
)
def test_serving_contract_invalid_sources_fail_before_any_selector_runs(
    loaded: dict[str, serving.Loaded], source: serving.Source, message: str
) -> None:
    with pytest.raises(serving.ServingError, match=message):
        _serve(loaded["S"], [source])
    with pytest.raises(serving.ServingError, match="duplicate"):
        good = serving.Source("P3", Path("x.go"), "x")
        _serve(loaded["S"], [good, good])
