"""Select, project, admit and score saved sources from one bundle.

Overlength candidates are withheld rather than truncated, because a partial candidate cannot support the same
verdict. P2 needs a caller-supplied target to certify package context. Read, selector and identity failures affect
one source; configuration, load and non-finite failures stop the exchange. Trace rows keep their per-arm shape
for comparison with reference reports.
"""

import dataclasses
import hashlib
import json
import math
import re
import tempfile
import time
from collections.abc import Mapping, Sequence
from pathlib import Path
from typing import Any

import torch

from stave_infer import backbone as bb
from stave_infer import bundle
from stave_infer import structure as ps
from stave_infer import tokenize as tk
from stave_infer import variants

SCHEMA = "stave-infer-serving-1"
LEDGER_TASKS = ("P2", "P3", "TF")
ELIGIBLE = {"P2": "eligible", "P3": "candidate"}
TF_SCHEMA = "stave-tf-ledger-1"
SELECTOR_ONLY_ENUMERATION = "selector results only; unselected constructs are unknown"
# The selector reports DC/RP withheld counts on stderr as a Go map.
DC_RP_WITHHELD = re.compile(r"stave-structural: (DC|RP) withheld map\[([^\]]*)\]")


class ServingError(ValueError):
    """A configuration, load or non-finite failure. No diagnostics are published."""


@dataclasses.dataclass(frozen=True)
class Source:
    """A saved source; `target` supplies P2 context and `tf_path` supplies the TF path gate."""

    task: str
    path: Path
    source_id: str
    target: Path | None = None
    tf_path: str | None = None


@dataclasses.dataclass(frozen=True)
class Loaded:
    """Model inputs and serving settings fixed by one verified bundle."""

    tokenizer: Any
    model: Any
    arm: str
    tasks: tuple[str, ...]
    seed: int
    thresholds: Mapping[str, float]
    batch_size: int
    budgets: Mapping[str, int]
    identity: Mapping[str, Any]
    load_seconds: float = 0.0


Row = tuple[dict[str, Any], dict[str, Any] | None]


def load(bundle_dir: Path, manifest: bundle.Manifest, *, threads: int | None) -> Loaded:
    """Load the verified tokenizer and the strict-loaded model of the bundle, ready to serve."""
    start = time.perf_counter()
    if threads is not None:
        if type(threads) is not int or threads < 1:
            raise ServingError(f"threads {threads!r} is not a positive integer")
        torch.set_num_threads(threads)
    m = manifest
    expected = bb.Expected(
        revision=m.backbone.revision,
        weight_bytes=m.backbone.weight_bytes,
        weight_sha256=m.backbone.weight_sha256,
        config_sha256=m.backbone.config_sha256,
        parameters=m.backbone.parameters,
    )
    try:
        bundle.verify_files(bundle_dir, m)
        tokenizer = tk.load_tokenizer(bundle_dir / m.tokenizer.dir, m.tokenizer.files)
        backbone = bb.build(bundle_dir / m.backbone.config)
        model = variants.load_model(
            m.variant, bundle_dir / m.model.file, backbone, expected, m.tasks, m.seed
        )
    except (ValueError, OSError, RuntimeError) as exc:
        raise ServingError(f"load failed: {exc}") from exc
    identity = {
        "bundle": {"id": m.id, "variant": m.variant},
        "model": {
            "path": str(bundle_dir / m.model.file),
            "format": m.model.format,
            "bytes": m.model.bytes,
            "sha256": m.model.sha256,
        },
        "backbone": dataclasses.asdict(expected),
        "tokenizer": {
            "dir": str(bundle_dir / m.tokenizer.dir),
            "files": dict(m.tokenizer.files),
        },
    }
    return Loaded(
        tokenizer, model, m.variant, m.tasks, m.seed, dict(m.thresholds), m.batch_size,
        dict(m.input_budgets), identity, time.perf_counter() - start,
    )  # fmt: skip


def check_sources(loaded: Loaded, sources: Sequence[Source]) -> None:
    """Check that every source names a served task, the right extras and a distinct ID."""
    seen = set()
    for s in sources:
        name = f"{s.task} source {s.source_id!r}"
        if s.task not in loaded.tasks:
            raise ServingError(f"{name}: task outside the inventory {loaded.tasks}")
        if s.target is not None and s.task != "P2":
            raise ServingError(f"{name}: only P2 takes a target")
        if (s.tf_path is not None) != (s.task == "TF"):
            raise ServingError(f"{name}: TF needs a tf_path and only TF takes one")
        if (s.task, s.source_id) in seen:
            raise ServingError(f"{name}: duplicate source ID")
        seen.add((s.task, s.source_id))


# Per-source selection and projection


def _need(condition: bool, message: str) -> None:
    if not condition:
        raise ps.StructureError(message)


def _row(
    row_id: str | None,
    status: str | None,
    anchor: Any,
    cand_span: Any,
    eligible: bool,
    streamed: bool,
    context: str | None = None,
) -> dict[str, Any]:
    return {
        "id": row_id,
        "ledger_status": status,
        "anchor": anchor,
        "cand_span": cand_span,
        "context": context,
        "eligible": eligible,
        "streamed": streamed,
        "projection": None,
        "projection_reason": None,
        "arms": {},
    }


def _select(
    source: Source, data: bytes, selector: Sequence[str]
) -> tuple[list[Row], dict[str, Any]]:
    """Return every visible row with its selector result, and the coverage of the file."""
    task, sha = source.task, hashlib.sha256(data).hexdigest()
    identity = {"bytes": len(data), "sha256": sha}
    stderr: list[str] = []
    with tempfile.TemporaryDirectory() as tmp:
        ledger_path = Path(tmp) / "ledger.json"
        command = list(selector)
        if task in LEDGER_TASKS:
            command += ["--ledger", str(ledger_path)]
        if source.target is not None:
            command += ["--target", str(source.target.absolute())]
        if task == "TF":
            assert source.tf_path is not None
            command += ["--path", source.tf_path, "--sha256", sha]
        results = ps.run_selector(command, task, data, source.source_id, stderr=stderr)
        ledger = json.loads(ledger_path.read_bytes()) if task in LEDGER_TASKS else None
    for r in results:
        _need(
            r.get("task") == task and r.get("source") == identity,
            f"{r.get('cand')}: result task {r.get('task')!r} or source "
            f"{r.get('source')} is not the served {task} {identity}",
        )
    if ledger is None:
        return _selector_only(task, results, stderr[0])
    expected = {"id": source.source_id, **identity}
    _need(
        ledger.get("source") == expected,
        f"ledger source {ledger.get('source')} is not the served {expected}",
    )
    if task == "TF":
        return _tf(ledger, results)
    return _records(task, ledger, results)


def _cand(result: Mapping[str, Any]) -> Any:
    return result["roles"]["CAND"]["iv"][0]


def _selector_only(
    task: str, results: list[dict[str, Any]], stderr: str
) -> tuple[list[Row], dict[str, Any]]:
    coverage: dict[str, Any] = {"enumeration": SELECTOR_ONLY_ENUMERATION}
    if task in ("DC", "RP"):
        found = DC_RP_WITHHELD.search(stderr)
        _need(
            found is not None and found.group(1) == task,
            f"{task} selector stderr has no withheld counts: {stderr!r}",
        )
        assert found is not None
        pairs = [item.rpartition(":") for item in found.group(2).split()]
        coverage["withheld"] = {reason: int(n) for reason, _, n in pairs}
    ids = [r["cand"] for r in results]
    _need(len(set(ids)) == len(ids), f"duplicate candidate IDs in {ids}")
    rows: list[Row] = [
        (_row(r["cand"], None, None, _cand(r) if r["ok"] else None, True, True), r)
        for r in results
    ]
    return rows, coverage


def _join(
    keyed: list[dict[str, Any]],
    results: list[dict[str, Any]],
    span: Any,
) -> dict[str, dict[str, Any]]:
    """Join the results one to one, in order, to the streamed ledger entries."""
    _need(
        len(keyed) == len(results),
        f"{len(results)} selector results for {len(keyed)} streamed ledger entries",
    )
    joined = {}
    for entry, result in zip(keyed, results, strict=True):
        key = entry.get("cand") if "id" not in entry else entry["id"]
        _need(
            result["cand"] == key, f"result {result['cand']} is not ledger entry {key}"
        )
        if result["ok"]:
            _need(
                _cand(result) == span(entry),
                f"{key}: result CAND {_cand(result)} is not the ledger CAND {span(entry)}",
            )
        joined[key] = result
    return joined


def _records(
    task: str, ledger: Mapping[str, Any], results: list[dict[str, Any]]
) -> tuple[list[Row], dict[str, Any]]:
    _need(
        ledger.get("task") == task, f"ledger task {ledger.get('task')!r} is not {task}"
    )
    records = ledger["records"]
    streamed = [r for r in records if "cand" in r]
    joined = _join(streamed, results, lambda r: r["cand"])
    rows = []
    for r in records:
        status, result = r["status"], joined.get(r["id"])
        eligible = status == ELIGIBLE[task]
        row = _row(
            r["id"], status, r["anchor"], r.get("cand"), eligible, result is not None
        )
        rows.append((row, result))
    coverage: dict[str, Any] = {"records": len(records)}
    if task == "P2":
        coverage["certificate"] = ledger.get("certificate")
    return rows, coverage


def _tf(
    ledger: Mapping[str, Any], results: list[dict[str, Any]]
) -> tuple[list[Row], dict[str, Any]]:
    _need(ledger.get("schema") == TF_SCHEMA, f"ledger schema {ledger.get('schema')!r}")
    gate = ledger["path_gate"]

    def span(row: Mapping[str, Any]) -> list[int]:
        return [row["target"]["start"], row["target"]["end"]]

    admitted = [r for r in ledger["rows"] if r.get("cand")]
    joined = _join(admitted, results, span)
    rows = []
    for r in ledger["rows"]:
        cand, context = r.get("cand") or None, r.get("context")
        status = "admitted" if r["admitted"] else r["reason"]
        eligible = cand is not None and gate["status"] == "eligible"
        row = _row(
            cand,
            status,
            span(r),
            span(r) if cand else None,
            eligible,
            cand in joined,
            context["status"] if context else None,
        )
        rows.append((row, joined.get(cand) if cand else None))
    coverage = {
        "path": ledger["path"],
        "path_gate": gate,
        "file": ledger["file"],
        "sentry": ledger["sentry"],
    }
    return rows, coverage


# Admission and scoring


def _score(
    loaded: Loaded,
    rows: list[Row],
    projections: dict[int, ps.Projection],
    threshold: float,
) -> None:
    arm, admitted = loaded.arm, []
    for i, (row, result) in enumerate(rows):
        p = projections.get(i)
        if p is not None:
            try:
                reason = variants.admit(arm, p)
            except variants.VariantError as exc:
                raise ServingError(str(exc)) from exc
        elif result is None:
            reason = f"not streamed: {row['ledger_status']}"
        else:
            reason = f"ineligible: {row['ledger_status']}"
        row["arms"][arm] = {
            "admission": "withheld" if reason else "admitted",
            "reason": reason,
            "logit": None,
            "logit_f32": None,
            "decision": None,
        }
        if reason is None:
            admitted.append(i)
    for start in range(0, len(admitted), loaded.batch_size):
        batch = admitted[start : start + loaded.batch_size]
        with torch.no_grad():
            out = loaded.model(variants.collate(arm, [projections[i] for i in batch]))
        logits = out.logits.to(torch.float32).contiguous()
        for i, value, bits in zip(
            batch, logits.tolist(), logits.view(torch.int32).tolist(), strict=True
        ):
            entry = rows[i][0]["arms"][arm]
            if not math.isfinite(value):
                raise ServingError(
                    f"{arm} {rows[i][0]['id']}: non-finite logit {value}"
                )
            entry.update(
                logit=value,
                logit_f32=f"{bits & 0xFFFFFFFF:08x}",
                decision=value >= threshold,
            )


def _serve(
    loaded: Loaded, source: Source, selector: Sequence[str], stages: dict[str, float]
) -> dict[str, Any]:
    file: dict[str, Any] = {
        "task": source.task,
        "source": {
            "id": source.source_id,
            "path": str(source.path),
            "bytes": None,
            "sha256": None,
        },
        "outcome": "ok",
        "error": None,
        "coverage": {},
        "rows": [],
    }
    stage, mark = "read", time.perf_counter()

    def lap(name: str) -> None:
        nonlocal stage, mark
        now = time.perf_counter()
        stages[stage] += now - mark
        stage, mark = name, now

    try:
        data = source.path.read_bytes()
        file["source"].update(bytes=len(data), sha256=hashlib.sha256(data).hexdigest())
        lap("select")
        rows, coverage = _select(source, data, selector)
        lap("project")
        budget = loaded.budgets[source.task]
        projections = {
            i: ps.project(loaded.tokenizer, data, result, max_tokens=budget)
            for i, (row, result) in enumerate(rows)
            if row["eligible"] and result is not None
        }
    except (
        ps.StructureError,
        OSError,
        ValueError,
        KeyError,
        IndexError,
        TypeError,
    ) as exc:
        lap(stage)
        return {**file, "outcome": "error", "error": f"{type(exc).__name__}: {exc}"}
    lap("score")
    for i, p in projections.items():
        rows[i][0].update(projection=p.status, projection_reason=p.reason)
    _score(loaded, rows, projections, loaded.thresholds[source.task])
    lap("read")
    return {**file, "coverage": coverage, "rows": [row for row, _ in rows]}


def _denominators(arm: str, files: Sequence[Mapping[str, Any]]) -> Any:
    """Count per task the visible rows, selector results, admitted rows and withheld rows by reason kind."""
    out: dict[str, Any] = {}
    for file in files:
        for row in file["rows"]:
            entry = row["arms"][arm]
            counts = out.setdefault(
                file["task"], {"rows": 0, "results": 0, "admitted": 0, "withheld": {}}
            )
            counts["rows"] += 1
            counts["results"] += row["streamed"]
            if entry["reason"] is None:
                counts["admitted"] += 1
            else:
                kind = entry["reason"].split(":")[0]
                counts["withheld"][kind] = counts["withheld"].get(kind, 0) + 1
    return {arm: out}


def diagnose(
    loaded: Loaded, sources: Sequence[Source], *, selector: Sequence[str]
) -> dict[str, Any]:
    """Return the diagnostics and coverage for every source, with the timing of this exchange."""
    check_sources(loaded, sources)
    started = time.perf_counter()
    stages = {"read": 0.0, "select": 0.0, "project": 0.0, "score": 0.0}
    files = [_serve(loaded, s, selector, stages) for s in sources]
    return {
        "schema": SCHEMA,
        "tasks": list(loaded.tasks),
        "seed": loaded.seed,
        "arm": loaded.arm,
        **loaded.identity,
        "selector": list(selector),
        "thresholds": dict(loaded.thresholds),
        "batch_size": loaded.batch_size,
        "max_tokens": dict(loaded.budgets),
        "files": files,
        "denominators": _denominators(loaded.arm, files),
        "errors": sum(f["outcome"] == "error" for f in files),
        "timing": {
            "unit": "perf_counter seconds in this process",
            "load_seconds": loaded.load_seconds,
            **stages,
            "total": time.perf_counter() - started,
        },
        "threads": {
            "intra_op": torch.get_num_threads(),
            "inter_op": torch.get_num_interop_threads(),
        },
    }
