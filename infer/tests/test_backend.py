"""Wire responses from all three toy variants.

The fixture copy stays outside `testdata` so the TF path gate permits it.
"""

import json
from pathlib import Path
from typing import Any

import pytest
from support import TOKENIZER_DIR, copy_fixture_package, selector, sha256

from stave_infer import backend, serving, toy

ARMS = ("B", "F", "S")
TASKS = ["P1", "P2", "P3", "TF", "DC", "RP"]


@pytest.fixture(scope="module")
def bundles(tmp_path_factory: pytest.TempPathFactory) -> dict[str, Any]:
    root = tmp_path_factory.mktemp("bundles")
    out = {}
    for arm in ARMS:
        manifest = toy.make_toy_bundle(arm, root / arm, tokenizer_dir=TOKENIZER_DIR)
        out[arm] = (manifest, serving.load(root / arm, manifest, threads=None))
    return out


def _request(*paths: Path, shas: dict[Path, str] | None = None) -> bytes:
    shas = shas or {}
    files = [
        {"path": str(p), "sha256": shas.get(p) or sha256(p.read_bytes())} for p in paths
    ]
    return json.dumps({"schema": "stave-advisory-v1", "files": files}).encode()


def _respond(bundles: dict[str, Any], arm: str, request: bytes, **kwargs: Any) -> Any:
    manifest, loaded = bundles[arm]
    return backend.respond(
        request, loaded=loaded, manifest=manifest, selector=selector(), **kwargs
    )


@pytest.mark.parametrize("arm", ARMS)
def test_one_request_gets_one_response_bound_to_the_served_rows(
    tmp_path: Path, bundles: dict[str, Any], arm: str
) -> None:
    pkg = copy_fixture_package(tmp_path)
    demo, inside, outside = (
        pkg / n for n in ("demo.go", "demo_test.go", "external_test.go")
    )
    (tmp_path / "other").mkdir()
    stale = (tmp_path / "other" / "stale.go").resolve()
    stale.write_bytes(b"package other\n")
    absent = (tmp_path / "other" / "absent.go").resolve()
    trace = tmp_path / "trace"
    trace.mkdir()
    request = _request(
        demo, inside, outside, stale, absent, shas={stale: "0" * 64, absent: "1" * 64}
    )
    response = _respond(bundles, arm, request, trace_dir=trace)
    manifest = bundles[arm][0]

    assert response["schema"] == "stave-advisory-v1" and response["classes"] == TASKS
    assert response["backend"] == {
        "id": f"toy-{arm}",
        "digest": manifest.model.sha256,
        "detail": f"variant {arm}, seed {manifest.seed}, thresholds "
        + json.dumps(dict(manifest.thresholds), sort_keys=True),
    }
    records = response["files"]
    assert [r["path"] for r in records] == [
        str(p) for p in (demo, inside, outside, stale, absent)
    ]
    assert records[3]["status"] == "error" and "stale" in records[3]["reason"]
    assert records[4]["status"] == "error" and "unreadable" in records[4]["reason"]
    assert all(
        p["path"] not in (str(stale), str(absent)) for p in response["predictions"]
    )

    package, *tests = records[:3]
    assert package["status"] == "checked"
    assert package["classes"] == ["P1", "P2", "P3", "DC", "RP"]
    # Test files must not enter the package digest used to validate P2 advice.
    demo_bytes = demo.read_bytes()
    assert package["context"] == sha256(
        b"demo.go\0" + sha256(demo_bytes).encode() + b"\n"
    )
    assert package["withheld"] == {"DC": {"selector withheld: no_prose": 1}}
    for record in tests:
        assert record["status"] == "checked" and record["classes"] == ["TF"]
        assert "context" not in record and "withheld" not in record

    (saved,) = trace.iterdir()
    report = json.loads(saved.read_text())
    assert report["schema"] == serving.SCHEMA and report["bundle"]["id"] == f"toy-{arm}"
    served = {}
    for file in report["files"]:
        for row in file["rows"]:
            key = (
                file["task"],
                file["source"]["path"],
                tuple(row["cand_span"] or row["anchor"]),
            )
            served[key] = row
    sources = {str(demo): demo_bytes, str(inside): inside.read_bytes(),
               str(outside): outside.read_bytes()}  # fmt: skip
    seen = set()
    for p in response["predictions"]:
        start, end = p["span"]["start"], p["span"]["end"]
        assert 0 <= start < end <= len(sources[p["path"]])
        assert p["sha256"] == sha256(sources[p["path"]])
        entry = served[(p["class"], p["path"], (start, end))]["arms"][arm]
        if p["coverage"] == "checked":
            assert p["score"] == entry["logit"]
            assert p["advice"] is (entry["logit"] >= manifest.thresholds[p["class"]])
            assert p["reason"] is None
        else:
            assert p["coverage"] == "unsupported" and p["reason"] == entry["reason"]
            assert p["advice"] is None and p["score"] is None
        seen.add((p["class"], sources[p["path"]][start:end]))
    for want in [
        ("P1", b"else"),
        ("P2", b'"Bad."'),
        ("P3", b"p_value"),
        ("DC", b"// T is a documented literal struct."),
        ("RP", b"(int, int)"),
        ("TF", b't.Error("inside")'),
        ("TF", b't.Error("outside")'),
    ]:
        assert want in seen, want
    assert len(served) == len(response["predictions"]), (
        "every served row with a span is reported"
    )


def test_a_test_file_under_a_testdata_directory_is_unsupported_by_the_tf_gate(
    bundles: dict[str, Any],
) -> None:
    from support import FIXTURE_PKG

    response = _respond(bundles, "S", _request(FIXTURE_PKG / "demo_test.go"))
    (record,) = response["files"]
    assert record["status"] == "unsupported"
    assert record["reason"] == "TF path gate excludes this path: testdata"
    assert response["predictions"] == []


def test_a_file_whose_task_fails_is_an_error_record(
    tmp_path: Path, bundles: dict[str, Any]
) -> None:
    broken = tmp_path / "broken.go"
    broken.write_bytes(b"package p\nfunc {")
    response = _respond(bundles, "B", _request(broken))
    (record,) = response["files"]
    assert record["status"] == "error" and "selector exited" in record["reason"]
    assert response["predictions"] == []


def test_an_empty_request_names_no_file_and_runs_nothing(
    bundles: dict[str, Any], tmp_path: Path
) -> None:
    trace = tmp_path / "trace"
    trace.mkdir()
    response = _respond(bundles, "F", _request(), trace_dir=trace)
    assert response["files"] == [] and response["predictions"] == []
    assert list(trace.iterdir()) == []


def test_a_malformed_request_raises_before_anything_is_served(
    bundles: dict[str, Any],
) -> None:
    with pytest.raises(ValueError, match="stave-advisory-v1"):
        _respond(bundles, "S", b'{"schema": "other"}')
