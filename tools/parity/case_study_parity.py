"""Check the converted S bundle against the frozen case-study sources and results.

    uv run --project infer --locked python tools/parity/case_study_parity.py \\
        --case-study CASE_STUDY_DIR --stave-home DATA_DIR --model BUNDLE_ID --out RESULT.json

Use the locked inference environment and a separate data directory with the converted S bundle installed. The script
sets that directory's thread count to `--threads` (default 12) and uses this environment's Python as the runtime.
Bundle provenance must identify the demonstrated checkpoint.

Network access is needed to clone the frozen repositories. Go and case-study source bytes must match
`SOURCE-FREEZE.json`; other file differences are recorded. Sibling files stay in place to preserve package context.

Diagnostics, judged candidates, decisions at threshold 0 and per-class coverage must match the retained evidence.
Scores and logits must match within tolerance. A traced second run includes negative predictions, whose
scores are absent from normal reports; both reports must match apart from the provider name.
Exit status 0 requires all source checks and report comparisons to pass.
"""

import argparse
import copy
import hashlib
import importlib.metadata
import json
import os
import subprocess
import sys
import tempfile
from collections.abc import Mapping, Sequence
from pathlib import Path
from typing import Any

REPO = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(REPO / "tools"))
from bundle.checkpoint_ids import product_task  # noqa: E402

ARM = "S"
THRESHOLD = 0.0
TOLERANCE = 1e-3
#: Must match SOURCE-FREEZE.json so source and package context stay fixed.
COMMITS = {
    "julienschmidt__httprouter": "484018016424d215c0b87c42f4c9b57d980fbd00",
    "tomnomnom__gron": "88a6234ea2d0c487090988182ad9a7cdf6def924",
}

Frozen = Mapping[str, tuple[str, str]]  # report path -> (repository, relative path)


def _sha256(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def _files(
    reports: Sequence[Mapping[str, Any]], frozen: Frozen
) -> list[tuple[Any, ...]]:
    """Return the reports of the frozen files, each with its repository and relative path."""
    return [
        (f, *frozen[f["path"]])
        for r in reports
        for f in r["files"]
        if f["path"] in frozen
    ]


def code_identities(
    reports: Sequence[Mapping[str, Any]], frozen: Frozen
) -> set[tuple[Any, ...]]:
    return {
        (
            d["class"],
            d["origin"],
            repo,
            rel,
            d["source_sha256"],
            d["span"]["start"],
            d["span"]["end"],
            d["context_digest"],
        )  # fmt: skip
        for f, repo, rel in _files(reports, frozen)
        for d in f["diagnostics"]
    }


def _findings(ledger: Mapping[str, Any]) -> list[Mapping[str, Any]]:
    return [f for f in ledger["findings"] if f["kind"] != "coverage"]


def ledger_identities(ledger: Mapping[str, Any]) -> set[tuple[Any, ...]]:
    """Return the code diagnostics of the baseline report of the case study as identities."""
    return {
        (
            product_task(f["class"]),
            f["origin"],
            f["repository"],
            f["relative_path"],
            f["data"]["source_sha256"],
            f["data"]["span"]["start"],
            f["data"]["span"]["end"],
            f["data"]["context_digest"],
        )  # fmt: skip
        for f in _findings(ledger)
    }


def score_deltas(
    reports: Sequence[Mapping[str, Any]], frozen: Frozen, ledger: Mapping[str, Any]
) -> dict[tuple[Any, ...], float]:
    """Return |Δ score| for each learned diagnostic that is in both the report and the ledger."""
    recorded = {
        (product_task(f["class"]), f["repository"], f["relative_path"], f["data"]["span"]["start"],
         f["data"]["span"]["end"]): f["data"]["score"]
        for f in _findings(ledger)
        if f["origin"] == "advisory"
    }  # fmt: skip
    out = {}
    for f, repo, rel in _files(reports, frozen):
        for d in f["diagnostics"]:
            key = (d["class"], repo, rel, d["span"]["start"], d["span"]["end"])
            if d["origin"] == "advisory" and key in recorded:
                out[key] = abs(d["score"] - recorded[key])
    return out


def coverage_counts(
    reports: Sequence[Mapping[str, Any]], frozen: Frozen
) -> dict[str, dict[str, int]]:
    """Count per class the candidates reported, judged and not judged, and the advisories, over the files."""
    out: dict[str, dict[str, int]] = {}
    for f, _, _ in _files(reports, frozen):
        for c in f["coverage"]:
            if c["origin"] != "advisory" or c.get("candidates") is None:
                continue  # an exact half, or a class not checked for this kind of file
            counts = out.setdefault(
                c["class"],
                {"reported": 0, "judged": 0, "not_judged": 0, "advised": 0},
            )
            counts["reported"] += c["candidates"]
            counts["not_judged"] += len(c["items"])
            counts["judged"] += c["candidates"] - len(c["items"])
            counts["advised"] += sum(
                d["origin"] == "advisory" and d["class"] == c["class"]
                for d in f["diagnostics"]
            )
    return out


def judged_logits(
    serving: Sequence[Mapping[str, Any]],
    frozen: Frozen,
    sha256: Mapping[str, str],
    arm: str = ARM,
    *,
    historical: bool = False,
) -> dict[tuple[Any, ...], float]:
    """Return the logit of every judged row, keyed by repository, file, task and CAND span.

    Only sources at a frozen path count. Each must have the frozen SHA-256.
    """
    out: dict[tuple[Any, ...], float] = {}
    for report in serving:
        for f in report["files"]:
            path = f["source"]["path"]
            if path not in frozen:
                continue
            repo, rel = frozen[path]
            if f["source"]["sha256"] != sha256[f"{repo}/{rel}"]:
                raise ValueError(
                    f"{path}: served sha256 {f['source']['sha256']} is not the frozen one"
                )
            for row in f["rows"]:
                entry = row["arms"][arm]
                if entry["admission"] != "admitted":
                    continue
                task = product_task(f["task"]) if historical else f["task"]
                key = (repo, rel, task, *row["cand_span"])
                if key in out and out[key] != entry["logit"]:
                    raise ValueError(
                        f"{key}: served twice with logits {out[key]} and {entry['logit']}"
                    )
                out[key] = entry["logit"]
    return out


def compare_judged(
    retained: Mapping[tuple[Any, ...], float],
    product: Mapping[tuple[Any, ...], float],
    tolerance: float,
) -> dict[str, Any]:
    shared = sorted(set(retained) & set(product))
    deltas = {k: abs(retained[k] - product[k]) for k in shared}
    beyond = [[*k, retained[k], product[k]] for k in shared if deltas[k] > tolerance]
    decisions = [
        [*k, retained[k], product[k]]
        for k in shared
        if (retained[k] >= THRESHOLD) != (product[k] >= THRESHOLD)
    ]
    only_retained = [list(k) for k in sorted(set(retained) - set(product))]
    only_product = [list(k) for k in sorted(set(product) - set(retained))]
    return {
        "judged": len(shared),
        "ok": not (beyond or decisions or only_retained or only_product),
        "max_abs_delta": max(deltas.values(), default=0.0),
        "beyond_tolerance": beyond,
        "decisions_differ": decisions,
        "only_retained": only_retained,
        "only_product": only_product,
    }


def same_but_provider(a: Mapping[str, Any], b: Mapping[str, Any]) -> bool:
    a, b = copy.deepcopy(dict(a)), copy.deepcopy(dict(b))
    a["advisory"]["provider"] = b["advisory"]["provider"] = None
    return a == b


# Run


def _run(argv: Sequence[str], env: Mapping[str, str] | None = None) -> str:
    done = subprocess.run(argv, env=env, capture_output=True, text=True, check=False)
    if done.returncode != 0:
        raise RuntimeError(f"{argv[:4]} exit {done.returncode}: {done.stderr.strip()}")
    return done.stdout


def _clone(freeze: Mapping[str, Any], work: Path) -> dict[str, Any]:
    """Verify frozen commits and Go/case-study files; record other file differences without failing."""
    verified: dict[str, Any] = {}
    for repo in freeze["repositories"]:
        name, commit = repo["repository"], repo["commit"]
        if COMMITS.get(name) != commit:
            raise RuntimeError(
                f"{name}: frozen commit {commit} is not {COMMITS.get(name)}"
            )
        target = work / name
        _run(["git", "clone", "--quiet", repo["origin"], str(target)])
        _run(
            [
                "git",
                "-C",
                str(target),
                "-c",
                "advice.detachedHead=false",
                "checkout",
                "--quiet",
                commit,
            ]
        )
        head = _run(["git", "-C", str(target), "rev-parse", "HEAD"]).strip()
        differ = [
            f["path"]
            for f in repo["files"]
            if not (target / f["path"]).is_file()
            or _sha256((target / f["path"]).read_bytes()) != f["sha256"]
        ]
        verified[name] = {"origin": repo["origin"], "head": head, "files": len(repo["files"]),
                          "differ": differ}  # fmt: skip
        if head != commit or any(p.endswith(".go") for p in differ):
            raise RuntimeError(
                f"{name}: HEAD {head} or Go files {differ} differ from the freeze"
            )
    for f in freeze["files"]:
        data = (work / f["repository"] / f["relative_path"]).read_bytes()
        if (len(data), _sha256(data)) != (f["bytes"], f["sha256"]):
            raise RuntimeError(
                f"{f['id']} {f['relative_path']}: bytes differ from the freeze"
            )
    return verified


def _reports(stdout: str) -> list[dict[str, Any]]:
    return [json.loads(line) for line in stdout.splitlines() if line.strip()]


def main(argv: Sequence[str] | None = None) -> int:
    parser = argparse.ArgumentParser(prog="case_study_parity.py", description=__doc__)
    parser.add_argument("--case-study", type=Path, required=True)
    parser.add_argument("--stave-home", type=Path, required=True)
    parser.add_argument(
        "--model", required=True, help="installed converted S bundle ID"
    )
    parser.add_argument("--stave", type=Path, default=REPO / "build" / "stave")
    parser.add_argument("--threads", type=int, default=12)
    parser.add_argument("--out", type=Path, required=True)
    args = parser.parse_args(argv)
    case, home, stave = args.case_study, args.stave_home.resolve(), args.stave.resolve()
    freeze = json.loads((case / "SOURCE-FREEZE.json").read_text())
    ledger = json.loads((case / "baseline-report-002" / "REPORT.json").read_text())
    audit = json.loads((case / "INDEPENDENT-BASELINE-AUDIT.json").read_text())
    runtime = json.loads((case / "RUNTIME.json").read_text())

    bundle = home / "models" / args.model
    manifest = json.loads((bundle / "manifest.json").read_text())
    if manifest["provenance"].get("source_model_sha256") != runtime["model_sha256"]:
        raise SystemExit(f"{bundle}: source checkpoint is not the demonstrated one")
    config_path = home / "config.json"
    config = json.loads(config_path.read_text())
    config["threads"] = args.threads
    config_path.write_text(json.dumps(config, indent=2) + "\n")

    result: dict[str, Any] = {
        "python": sys.version.split()[0],
        "torch": importlib.metadata.version("torch"),
        "transformers": importlib.metadata.version("transformers"),
        "stave": str(stave),
        "stave_home": str(home),
        "config": config,
        "bundle": {"id": manifest["id"], "model_sha256": manifest["model"]["sha256"]},
    }
    with tempfile.TemporaryDirectory() as tmp:
        work = Path(tmp).resolve()
        result["repositories"] = _clone(freeze, work / "src")
        paths = [
            str(work / "src" / f["repository"] / f["relative_path"])
            for f in freeze["files"]
        ]
        frozen = {
            p: (f["repository"], f["relative_path"])
            for p, f in zip(paths, freeze["files"])
        }
        env = {
            **os.environ,
            "STAVE_HOME": str(home),
            "STAVE_RUNTIME_PYTHON": sys.executable,
        }
        check = [str(stave), "check", "-json", "-model", args.model, *paths]
        reports = _reports(_run(check, env))

        trace = work / "trace"
        trace.mkdir()
        serve = [
            sys.executable, "-m", "stave_infer", "serve", "--bundle", str(bundle),
            "--selector", str(stave), "--threads", str(args.threads), "--trace-dir", str(trace),
        ]  # fmt: skip
        traced_check = [
            str(stave), "check", "-json", "-advisory", "command",
            "-advisory-cmd", json.dumps(serve), *paths,
        ]  # fmt: skip
        traced = _reports(_run(traced_check, env))
        traces = [
            json.loads(p.read_text()) for p in sorted(trace.glob("serving-*.json"))
        ]
        result["commands"] = {"check": check, "traced_check": traced_check}

        identities, recorded = (
            code_identities(reports, frozen),
            ledger_identities(ledger),
        )
        deltas = score_deltas(reports, frozen, ledger)
        advice = [
            d["score"]
            for f, _, _ in _files(reports, frozen)
            for d in f["diagnostics"]
            if d["origin"] == "advisory"
        ]
        result["diagnostics"] = {
            "ok": identities == recorded
            and len(deltas) == len(advice)
            and all(s >= THRESHOLD for s in advice)
            and max(deltas.values(), default=0.0) <= TOLERANCE,
            "count": len(identities),
            "recorded": len(recorded),
            "by_origin_class": {
                k: sum(f"{i[1]}/{i[0]}" == k for i in identities)
                for k in sorted({f"{i[1]}/{i[0]}" for i in identities})
            },
            "only_product": sorted(map(list, identities - recorded)),
            "only_recorded": sorted(map(list, recorded - identities)),
            "advisory_scores_max_abs_delta": max(deltas.values(), default=0.0),
        }
        result["traced_report_equal"] = len(reports) == len(traced) and all(
            same_but_provider(a, b) for a, b in zip(reports, traced, strict=False)
        )
        result["advisory"] = [r["advisory"] for r in reports]
        sha_by_file = {
            f"{f['repository']}/{f['relative_path']}": f["sha256"]
            for f in freeze["files"]
        }
        retained_frozen = {
            f["path"]: (f["repository"], f["relative_path"]) for f in freeze["files"]
        }
        retained = judged_logits(
            [
                json.loads(p.read_text())
                for p in sorted((case / "serving").glob("serving-*.json"))
            ],
            retained_frozen,
            sha_by_file,
            historical=True,
        )
        product = judged_logits(traces, frozen, sha_by_file)
        result["logits"] = compare_judged(retained, product, TOLERANCE)
        result["trace_threads"] = sorted({json.dumps(t["threads"]) for t in traces})
        result["exchanges"] = len(traces)
        counts = coverage_counts(reports, frozen)
        expected = {
            label: {"reported": c["reported"], "judged": c["admitted"],
                    "not_judged": c["not_judged"], "advised": c["advised"]}
            for label, c in audit["candidate_counts"].items()
        }  # fmt: skip
        result["coverage"] = {
            "ok": counts == expected,
            "product": counts,
            "case_study": expected,
        }

    ok = (
        result["diagnostics"]["ok"]
        and result["traced_report_equal"]
        and result["logits"]["ok"]
        and result["coverage"]["ok"]
        and all(a["status"] == "ok" for a in result["advisory"])
    )
    result["ok"] = ok
    args.out.write_text(json.dumps(result, indent=2) + "\n")
    print(
        f"{'PASS' if ok else 'FAIL'}: diagnostics {result['diagnostics']['count']}/"
        f"{result['diagnostics']['recorded']} ok={result['diagnostics']['ok']}; judged "
        f"{result['logits']['judged']} max|Δlogit|={result['logits']['max_abs_delta']:.3g} "
        f"ok={result['logits']['ok']}; coverage ok={result['coverage']['ok']}; traced report "
        f"equal={result['traced_report_equal']}"
    )
    return 0 if ok else 1


if __name__ == "__main__":
    sys.exit(main())
