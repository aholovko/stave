"""Token facts, role membership and F/S masks from real selector results."""

import hashlib
import math
import re
from pathlib import Path
from typing import Any

import pytest
import support

from stave_infer import structure as ps
from stave_infer import tokenize as tk

# Toy byte-level tokenizer
# Leading whitespace makes tokens cross fact and role boundaries, exercising the last-byte rule.


class WordTokenizer:
    cls_token = "<s>"
    sep_token = "</s>"
    unk_token = "<unk>"
    _pattern = re.compile(rb"\s*\w+|\s*[^\w\s]+|\s+")

    def __init__(self) -> None:
        self.byte_decoder = {chr(0x100 + b): b for b in range(256)}
        self.added_tokens_encoder = {"<s>": 0, "<pad>": 1, "</s>": 2, "<unk>": 3}

    def tokenize(self, text: str) -> list[str]:
        return [
            "".join(chr(0x100 + b) for b in m.group())
            for m in self._pattern.finditer(text.encode("utf-8"))
        ]

    def convert_tokens_to_ids(self, tokens: list[str]) -> list[int]:
        return [self.added_tokens_encoder.get(t, 5) for t in tokens]


TOK = WordTokenizer()
BUDGET = 1023

# Hand-projected P1 result

SRC = (
    b"package p\n\nfunc F(a int) int {\n\tif a > 0 {\n\t\treturn 1\n"
    b"\t} else {\n\t}\n\treturn a\n}\n"
)


def _at(needle: bytes, start: int = 0) -> int:
    return SRC.index(needle, start)


SIG = (_at(b"func"), _at(b" {"))
IF = (_at(b"if"), _at(b"}\n\treturn a") + 1)
THEN = (_at(b"{", IF[0]), _at(b"} else") + 1)
RET = (_at(b"return 1"), _at(b"return 1") + len(b"return 1"))
ELSE_KW = (_at(b"else"), _at(b"else") + 4)
ELSE = (_at(b"{", ELSE_KW[1]), IF[1])


def _p1_result(**overrides: Any) -> dict[str, Any]:
    result: dict[str, Any] = {
        "ok": True,
        "task": "P1",
        "cand": "P1:fx:0",
        "source": {"bytes": len(SRC), "sha256": hashlib.sha256(SRC).hexdigest()},
        "runs": [list(SIG), list(IF)],
        "roles": {
            "CAND": {"iv": [list(ELSE_KW)], "why": "present"},
            "IF_HDR": {"iv": [[IF[0], THEN[0]]], "why": "present"},
            "THEN_BODY": {"iv": [list(THEN)], "why": "present"},
            "THEN_LAST": {"iv": [list(RET)], "why": "present"},
            "ELSE_BODY": {"iv": [list(ELSE)], "why": "present"},
            "FUNC_SIG": {"iv": [list(SIG)], "why": "present"},
        },
        "facts": [
            [*SIG, "NoStmt", "FuncRoot", None, None, None, None, 0],
            [IF[0], THEN[0], "If", "Outside", None, None, None, 0, 0],
            [THEN[0], RET[0], "If", "Outside", "Then", None, None, 0, 0],
            [*RET, "Return", "If", "Then", 0, "Last", 1, 0],
            [RET[1], THEN[1], "If", "Outside", "Then", None, None, 0, 0],
            [THEN[1], ELSE[0], "If", "Outside", None, None, None, 0, 0],
            [*ELSE, "If", "Outside", "Else", None, None, 0, 0],
        ],
    }
    result.update(overrides)
    return result


def _project(result: dict[str, Any], **kwargs: Any) -> ps.Projection:
    return ps.project(TOK, SRC, result, **{"max_tokens": BUDGET, **kwargs})


def _column(inventory: tuple[str, ...], value: str) -> int:
    return inventory.index(value)


def _spelled(data: bytes) -> str:
    return "".join(chr(0x100 + b) for b in data)


def test_inventories_keep_the_frozen_bucket_counts_and_the_six_role_sets() -> None:
    assert ps.BUCKETS == (22, 24, 6, 10, 3, 9, 4, 4)
    assert {task: len(roles) for task, roles in ps.ROLES.items()} == {
        "P1": 6,
        "P2": 2,
        "P3": 2,
        "TF": 8,
        "DC": 2,
        "RP": 3,
    }
    assert list(ps.ROLES) == list(ps.REQUIRED) == ["P1", "P2", "P3", "TF", "DC", "RP"]
    assert all(roles[0] == "CAND" for roles in ps.ROLES.values())


def test_tf_roles_and_required_set_are_the_reviewed_inventories() -> None:
    assert ps.ROLES["TF"] == (
        "CAND",
        "ARGS",
        "PREV",
        "ENCL_HDR",
        "RUN_PREFIX",
        "BIND_SIG",
        "FUNC_SIG",
        "BODY",
    )
    assert ps.REQUIRED["TF"] == ("CAND", "ARGS", "BIND_SIG", "FUNC_SIG", "BODY")


TF_SRC = (
    b'package p\n\nimport "testing"\n\nfunc TestA(t *testing.T) {\n'
    b'\tt.Error("")\n\tt.Errorf("got %d", 1)\n}\n'
)


def test_tf_required_args_without_token_differs_from_empty_optional_roles(
    tmp_path: Path,
) -> None:
    sha = hashlib.sha256(TF_SRC).hexdigest()
    flags = ["--ledger", str(tmp_path / "l.json"), "--path", "x_test.go"]
    command = [*support.selector(), *flags, "--sha256", sha]
    results = ps.run_selector(command, "TF", TF_SRC, "fx")
    empty, later = (ps.project(TOK, TF_SRC, r, max_tokens=512) for r in results)
    assert empty.status == "incomplete" and "ARGS" in (empty.reason or "")
    assert empty.role_status["ARGS"] == "no_token"
    for role in ("PREV", "ENCL_HDR", "RUN_PREFIX"):
        assert empty.role_status[role] == "structural_empty"
    assert later.status == "admitted"
    assert later.role_status["PREV"] == later.role_status["ARGS"] == "present"
    facts, mu, f_mask = later.arm("F")
    assert later.arm("S")[:2] == (facts, mu) and later.arm("S")[2] != f_mask


def test_seam_and_frame_rows_carry_no_structure_and_no_role() -> None:
    p = _project(_p1_result())
    assert p.status == "admitted" and p.facts is not None and p.mu is not None
    segment = [row[7] for row in p.facts]
    seams = [i for i, s in enumerate(segment) if s == _column(ps.SEGMENTS, "Sep")]
    assert len(seams) == 1
    assert p.facts[seams[0]][:7] == [
        _column(ps.KINDS, "Sep"),
        _column(ps.PARENTS, "Sep"),
        _column(ps.SLOTS, "None"),
        _column(ps.ORDINALS, "None"),
        _column(ps.LASTS, "None"),
        _column(ps.DEPTHS, "None"),
        _column(ps.LITS, "None"),
    ]
    framing = [i for i, s in enumerate(segment) if s == _column(ps.SEGMENTS, "Frame")]
    assert framing == [0, 1, 2, p.tokens - 1]
    assert _column(ps.SEGMENTS, "Pad") not in segment
    for i in (*seams, *framing):
        assert not any(p.mu[i])
        if i not in seams:
            assert p.facts[i][:2] == [
                _column(ps.KINDS, "NoStmt"),
                _column(ps.PARENTS, "None"),
            ]


def test_token_takes_the_facts_of_its_last_byte_owner() -> None:
    p = _project(_p1_result())
    assert p.facts is not None and p.encoding is not None
    rows = {t.token: row for t, row in zip(p.encoding.tokens, p.facts, strict=True)}
    # "\n\t\treturn" starts in If-owned Then bytes and ends in the Return frame.
    assert rows[_spelled(b"\n\t\treturn")][:7] == [
        _column(ps.KINDS, "Return"),
        _column(ps.PARENTS, "If"),
        _column(ps.SLOTS, "Then"),
        _column(ps.ORDINALS, "0"),
        _column(ps.LASTS, "Last"),
        _column(ps.DEPTHS, "1"),
        _column(ps.LITS, "Root"),
    ]


def test_candidate_bytes_stay_exact_while_tokens_are_chosen_by_last_byte() -> None:
    p = _project(_p1_result())
    assert p.candidate_span == ELSE_KW and p.mu is not None and p.encoding is not None
    cand = ps.ROLES["P1"].index("CAND")
    chosen = [t for t, row in zip(p.encoding.tokens, p.mu, strict=True) if row[cand]]
    assert [SRC[t.start : t.end] for t in chosen] == [b" else"]
    assert chosen[0].start is not None and chosen[0].start < ELSE_KW[0]


def test_disjoint_role_union_never_uses_its_hull() -> None:
    roles = dict(_p1_result()["roles"])
    roles["IF_HDR"] = {
        "iv": [[IF[0], IF[0] + 2], [THEN[0] - 1, THEN[0]]],
        "why": "present",
    }
    p = _project(_p1_result(roles=roles))
    assert p.mu is not None and p.encoding is not None
    hdr = ps.ROLES["P1"].index("IF_HDR")
    chosen = [
        SRC[t.start : t.end]
        for t, row in zip(p.encoding.tokens, p.mu, strict=True)
        if row[hdr]
    ]
    assert chosen == [b"if"]


def test_arms_share_facts_and_all_source_s_mask_reproduces_f() -> None:
    p = _project(_p1_result())
    f_facts, f_mu, f_mask = p.arm("F")
    s_facts, s_mu, s_mask = p.arm("S")
    assert f_facts is s_facts and f_mu is s_mu and f_mask != s_mask
    assert p.encoding is not None
    source = [t.start is not None for t in p.encoding.tokens]
    assert ps.additive_mask([source] * len(ps.ROLES["P1"])) == f_mask
    for r in range(len(ps.ROLES["P1"])):
        for v, row in zip(s_mask[r], s_mu, strict=True):
            assert (v == 0.0) == bool(row[r])
        for v, is_source in zip(f_mask[r], source, strict=True):
            assert (v == 0.0) == is_source and v in (0.0, -math.inf)


def test_statuses_distinguish_empty_roles_malformed_and_unassembled() -> None:
    roles = dict(_p1_result()["roles"])
    roles["THEN_LAST"] = {"iv": [], "why": "structural_empty"}
    p = _project(_p1_result(roles=roles))
    assert p.status == "admitted" and p.role_status["THEN_LAST"] == "structural_empty"
    failed = {
        "ok": False,
        "task": "P1",
        "cand": "P1:fx:0",
        "source": _p1_result()["source"],
        "reason": "bad event",
        "event_index": 3,
    }
    m = _project(failed)
    assert (m.status, m.reason, m.facts, m.mu) == ("malformed", "bad event", None, None)
    long = _project(_p1_result(), max_tokens=10)
    assert long.status == "unassembled" and long.facts is None
    assert long.reason is not None and long.reason.endswith("exceed 10")


def test_required_role_without_a_token_is_not_admitted() -> None:
    roles = dict(_p1_result()["roles"])
    roles["IF_HDR"] = {"iv": [[IF[0], IF[0] + 1]], "why": "present"}
    p = _project(_p1_result(roles=roles))
    assert p.status == "incomplete" and p.role_status["IF_HDR"] == "no_token"
    for arm in ("F", "S"):
        with pytest.raises(ps.StructureError, match="incomplete"):
            p.arm(arm)


def test_source_identity_is_verified_before_assembly() -> None:
    with pytest.raises(ps.StructureError, match="identity"):
        ps.project(TOK, SRC + b"\n", _p1_result(), max_tokens=BUDGET)


def test_facts_must_cover_the_runs_exactly() -> None:
    facts = _p1_result()["facts"][:-1]
    with pytest.raises(ps.StructureError, match="not covered"):
        _project(_p1_result(facts=facts))


def test_a_task_outside_the_six_is_refused() -> None:
    with pytest.raises(ps.StructureError, match="unknown task"):
        _project(_p1_result(task="DS"))


# Fixture package through the selector and tokenizer

#: Verify the first candidate's source bytes, independently of tokenization.
FIXTURE_CANDIDATES = {
    "P1": b"else",
    "P2": b'"Bad."',
    "P3": b"p_value",
    "DC": b"// T is a documented literal struct.",
    "RP": b"(int, int)",
    "TF": b't.Error("inside")',
}
BUDGETS = {"P1": 1023, "P2": 1023, "P3": 1023, "TF": 512, "DC": 512, "RP": 512}


@pytest.fixture(scope="module")
def unixcoder(tmp_path_factory: pytest.TempPathFactory) -> Any:
    directory = support.copy_tokenizer(tmp_path_factory.mktemp("tokenizer"))
    return tk.load_tokenizer(directory, support.tokenizer_files())


@pytest.mark.parametrize("task", list(FIXTURE_CANDIDATES))
def test_fixture_package_projects_for_each_of_the_six_tasks(
    tmp_path: Path, unixcoder: Any, task: str
) -> None:
    name = "demo_test.go" if task == "TF" else "demo.go"
    path = support.FIXTURE_PKG / name
    data = path.read_bytes()
    command = support.selector()
    if task in ("P2", "P3", "TF"):
        command += ["--ledger", str(tmp_path / "ledger.json")]
    if task == "P2":
        command += ["--target", str(path)]
    if task == "TF":
        command += ["--path", f"fixture-pkg/{name}", "--sha256", support.sha256(data)]
    results = ps.run_selector(command, task, data, "f0")
    assert results and all(r["ok"] for r in results)
    projections = [
        ps.project(unixcoder, data, r, max_tokens=BUDGETS[task]) for r in results
    ]
    spans = {}
    for p in projections:
        assert p.status == "admitted", (p.candidate, p.reason)
        assert p.candidate_span is not None and p.encoding is not None
        start, end = p.candidate_span
        spans[data[start:end]] = p
        assert p.tokens == p.encoding.total_tokens <= BUDGETS[task]
    assert FIXTURE_CANDIDATES[task] in spans


def test_run_selector_appends_stderr_and_fails_on_a_nonzero_exit() -> None:
    data = (support.FIXTURE_PKG / "demo.go").read_bytes()
    captured: list[str] = []
    results = ps.run_selector(support.selector(), "DC", data, "f0", stderr=captured)
    assert results == ps.run_selector(support.selector(), "DC", data, "f0")
    assert captured == ["stave-structural: DC withheld map[no_prose:1]\n"]
    with pytest.raises(ps.StructureError, match="selector exited 1"):
        ps.run_selector(support.selector(), "P1", b"package p\nfunc {", "x")
