"""Map served rows onto `stave-advisory-v1`.

Test files serve TF only; other files serve P1, P2, P3, DC and RP. Each record declares its classes so consumers
can distinguish unchecked classes from negative predictions. Scores are raw float32 logits, not probabilities.
P2 carries the selector's context digest so the harness can reject advice from a different package snapshot.
`backend.id` is the bundle id and `digest` the SHA-256 of `model.pt`, so a report names the model that produced it.
"""

import hashlib
import json
import os
import time
from collections.abc import Sequence
from pathlib import Path
from typing import Any

from stave_infer import bundle, serving
from stave_infer.request import SCHEMA, RequestFile, parse_request

TASKS = bundle.TASKS
PACKAGE_TASKS = ("P1", "P2", "P3", "DC", "RP")


def _record(file: RequestFile, status: str, reason: str | None) -> dict[str, Any]:
    return {
        "path": file.path,
        "sha256": file.sha256,
        "status": status,
        "reason": reason,
    }


def _span(row: dict[str, Any]) -> dict[str, int] | None:
    span = row["cand_span"] or row["anchor"]
    return None if span is None else {"start": span[0], "end": span[1]}


def _file(
    file: RequestFile, served: dict[str, dict[str, Any]], arm: str
) -> tuple[dict[str, Any], list[dict[str, Any]]]:
    """Build the record and predictions of one requested file from its served tasks."""
    errors = [
        f"{t}: {s['error']}" for t, s in served.items() if s["outcome"] == "error"
    ]
    errors += [
        f"{t}: the file changed while it was served"
        for t, s in served.items()
        if s["outcome"] != "error" and s["source"]["sha256"] != file.sha256
    ]
    if errors:
        return _record(file, "error", "; ".join(errors)), []
    if "TF" in served:
        coverage = served["TF"]["coverage"]
        gate, row = coverage["path_gate"], coverage["file"]
        if gate["status"] != "eligible":
            return _record(
                file,
                "unsupported",
                f"TF path gate excludes this path: {gate['reason']}",
            ), []
        if row["status"] != "checked":
            return _record(
                file,
                "unsupported",
                f"TF file status {row['status']}: {row.get('reason')}",
            ), []

    record = _record(file, "checked", None)
    record["classes"] = list(served)
    withheld: dict[str, dict[str, int]] = {}

    def withhold(task: str, reason: str, n: int = 1) -> None:
        counts = withheld.setdefault(task, {})
        counts[reason] = counts.get(reason, 0) + n

    predictions = []
    for task, s in served.items():
        coverage = s["coverage"]
        if task == "P2" and coverage.get("certificate"):
            record["context"] = coverage["certificate"]["file_set_digest"]
        for reason, n in coverage.get("withheld", {}).items():
            withhold(task, f"selector withheld: {reason}", n)
        for hit in coverage.get("sentry", {}).get("hits", []):
            if hit["status"] != "row":
                withhold(task, f"unresolved TF sentry hit: {hit.get('reason')}")
        for row in s["rows"]:
            entry, span = row["arms"][arm], _span(row)
            if entry["admission"] == "admitted":
                prediction = {
                    "coverage": "checked", "advice": entry["decision"],
                    "score": entry["logit"], "reason": None,
                }  # fmt: skip
            elif span is None:
                withhold(task, entry["reason"])
                continue
            else:
                prediction = {
                    "coverage": "unsupported", "advice": None, "score": None,
                    "reason": entry["reason"],
                }  # fmt: skip
            predictions.append(
                {
                    "path": file.path,
                    "class": task,
                    "sha256": file.sha256,
                    "span": span,
                    **prediction,
                }
            )
    if withheld:
        record["withheld"] = withheld
    return record, predictions


def respond(
    request: bytes,
    *,
    loaded: serving.Loaded,
    manifest: bundle.Manifest,
    selector: Sequence[str],
    trace_dir: Path | None = None,
) -> dict[str, Any]:
    """Answer one request with one response. Raise ValueError or OSError when none can be given."""
    requested = parse_request(request)
    records: dict[int, dict[str, Any]] = {}
    sources: list[serving.Source] = []
    owners: list[int] = []
    for i, file in enumerate(requested):
        try:
            digest = hashlib.sha256(Path(file.path).read_bytes()).hexdigest()
        except OSError as exc:
            records[i] = _record(file, "error", f"unreadable: {exc}")
            continue
        if digest != file.sha256:
            records[i] = _record(
                file,
                "error",
                f"stale: the file on disk has sha256 {digest}, not the requested {file.sha256}",
            )
            continue
        tasks = ("TF",) if file.path.endswith("_test.go") else PACKAGE_TASKS
        for task in tasks:
            path = Path(file.path)
            sources.append(serving.Source(
                task, path, f"f{i}",
                target=path if task == "P2" else None,
                tf_path=file.path if task == "TF" else None,
            ))  # fmt: skip
            owners.append(i)
    predictions: dict[int, list[dict[str, Any]]] = {}
    if sources:
        report = serving.diagnose(loaded, sources, selector=selector)
        if trace_dir is not None:
            name = f"serving-{time.time_ns()}-{os.getpid()}.json"
            (trace_dir / name).write_text(json.dumps(report, allow_nan=False) + "\n")
        grouped: dict[int, dict[str, dict[str, Any]]] = {}
        for owner, source, served in zip(owners, sources, report["files"], strict=True):
            grouped.setdefault(owner, {})[source.task] = served
        for i, served in grouped.items():
            records[i], predictions[i] = _file(requested[i], served, loaded.arm)
    thresholds = json.dumps(dict(manifest.thresholds), sort_keys=True)
    return {
        "schema": SCHEMA,
        "backend": {
            "id": manifest.id,
            "digest": manifest.model.sha256,
            "detail": f"variant {manifest.variant}, seed {manifest.seed}, thresholds {thresholds}",
        },
        "classes": list(TASKS),
        "files": [records[i] for i in range(len(requested))],
        "predictions": [
            p for i in range(len(requested)) for p in predictions.get(i, [])
        ],
    }
