"""Compare converted B/F/S bundles with the original research checkpoints and predictions.

    uv run --project /path/to/stave-research --locked --all-groups --extra cpu \\
        python tools/parity/loader_parity.py --infer-src infer/src --run RUN_DIR \\
        --model-dir PRETRAINED_DIR --tokenizer-dir DIR --tokenizer-receipt PATH \\
        --bundle S=DIR --bundle F=DIR --bundle B=DIR \\
        --research-selector EXE --stave EXE --out RESULT.json

Use the locked research environment, original pretrained backbone and tokenizer, research selector and current STAVE
binary. Product code comes from `--infer-src`; the research checkout stays unchanged.

After task-ID conversion, tensors must have identical shapes, dtypes and values, and judged float32 logits must match
bit for bit. Normalized per-file reports must match without serving errors. Both paths use the same fixture sources
outside `testdata` so the TF path gate permits them. Bundle provenance must identify the original checkpoint.

B's saved `init.sha256` depends on the training host's CPU architecture. If that check refuses B, the script rebuilds
the research reference with strict tensor loading and seed/inventory checks. It records the refusal and every
configuration difference; only `init.sha256` may differ. Exit status 0 requires every variant to pass.
"""

import argparse
import gc
import hashlib
import json
import os
import shutil
import sys
import tempfile
from collections.abc import Mapping, Sequence
from pathlib import Path
from typing import Any

REPO = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(REPO / "tools"))
from bundle.checkpoint_ids import TASK_IDS, product_task, state_key  # noqa: E402

PACKAGE_TASKS = ("P1", "P2", "P3", "DC", "RP")
RESEARCH_IDS = {p: e for e, p in TASK_IDS.items()}
CONFIG_REFUSAL = "saved config differs from the rebuilt model"


def compare_states(
    research: Mapping[str, Any], product: Mapping[str, Any]
) -> dict[str, Any]:
    """Compare two state dicts tensor for tensor: dtype, shape and values."""
    import torch

    research = {state_key(k): v for k, v in research.items()}
    shared = sorted(set(research) & set(product))
    differ = [
        k
        for k in shared
        if research[k].dtype != product[k].dtype
        or not torch.equal(research[k], product[k])
    ]
    missing = sorted(set(product) - set(research))
    extra = sorted(set(research) - set(product))
    return {
        "tensors": len(shared),
        "equal": not (differ or missing or extra),
        "differ": differ,
        "missing": missing,
        "extra": extra,
    }


def product_records(value: Any) -> Any:
    """Translate historical task fields and candidate prefixes, preserving all other report content."""
    if isinstance(value, list):
        return [product_records(item) for item in value]
    if not isinstance(value, dict):
        return value
    out = {}
    for key, item in value.items():
        if key in ("task", "class") and isinstance(item, str):
            out[key] = product_task(item)
        elif key in ("id", "cand") and isinstance(item, str) and ":" in item:
            task, suffix = item.split(":", 1)
            out[key] = product_task(task) + ":" + suffix
        else:
            out[key] = product_records(item)
    return out


def _judged(files: Sequence[Mapping[str, Any]], arm: str) -> dict[tuple[Any, ...], str]:
    out = {}
    for f in files:
        for row in f["rows"]:
            entry = row["arms"][arm]
            if entry["admission"] == "admitted":
                key = (f["source"]["id"], f["task"], row["id"], tuple(row["cand_span"]))
                out[key] = entry["logit_f32"]
    return out


def compare_logits(
    research: Sequence[Mapping[str, Any]],
    product: Sequence[Mapping[str, Any]],
    arm: str,
) -> dict[str, Any]:
    """Compare the float32 logits of the judged rows bit for bit, row by row."""
    r, p = _judged(product_records(research), arm), _judged(product, arm)

    def listed(keys: Any) -> list[list[Any]]:
        return [[*k[:3], list(k[3])] for k in sorted(keys)]

    differ = [
        [*k[:3], list(k[3]), r[k], p[k]]
        for k in sorted(set(r) & set(p))
        if r[k] != p[k]
    ]
    only_research, only_product = listed(set(r) - set(p)), listed(set(p) - set(r))
    return {
        "judged": len(set(r) & set(p)),
        "equal": not (differ or only_research or only_product),
        "differ": differ,
        "only_research": only_research,
        "only_product": only_product,
    }


def sources(source: Any, package: Path, *, research: bool = False) -> list[Any]:
    """Mirror backend task selection so both loaders receive the same inputs."""
    out = []
    for i, path in enumerate(sorted(package.glob("*.go"))):
        tasks = ("TF",) if path.name.endswith("_test.go") else PACKAGE_TASKS
        for task in tasks:
            out.append(
                source(
                    RESEARCH_IDS.get(task, task) if research else task,
                    path,
                    f"f{i}",
                    target=path if task == "P2" else None,
                    tf_path=str(path) if task == "TF" else None,
                )
            )
    return out


def _leaves(value: Any, prefix: str = "") -> dict[str, Any]:
    if not isinstance(value, dict) or not value:
        return {prefix: value}
    out = {}
    for key, item in value.items():
        out.update(_leaves(item, f"{prefix}.{key}" if prefix else key))
    return out


def config_differences(
    saved: Mapping[str, Any], rebuilt: Mapping[str, Any]
) -> dict[str, dict[str, Any]]:
    """Return every dotted configuration field whose saved and rebuilt values differ."""
    a, b = _leaves(dict(saved)), _leaves(dict(rebuilt))
    return {
        k: {"saved": a.get(k), "rebuilt": b.get(k)}
        for k in sorted(set(a) | set(b))
        if a.get(k) != b.get(k)
    }


def research_reference(
    arm: str,
    path: Path,
    model_dir: Path,
    expected: Any,
    tasks: Sequence[str],
    seed: int,
    *,
    flat: Any,
    checkpoint: Any,
) -> tuple[Any, dict[str, Any]]:
    """Rebuild the reference model and report configuration differences for caller verification.

    The caller accepts only the architecture-dependent initialization digest difference.
    """
    import torch

    if arm != "B":
        raise ValueError(f"the configuration fallback is only B's, not {arm}'s")
    saved = torch.load(path, map_location="cpu", weights_only=True)
    saved_tasks, saved_seed = flat._saved_inventory(path, saved)
    backbone, _ = checkpoint.load_checked(model_dir, expected)
    model = flat.FlatModel(backbone, tasks=saved_tasks, seed=saved_seed)
    differs = config_differences(
        json.loads(saved["config_json"]), json.loads(flat.config_json(model, expected))
    )
    state = saved["state"]

    def inventory(tensors: Any) -> dict[str, Any]:
        return {
            k: (v.dtype, tuple(v.shape)) if isinstance(v, torch.Tensor) else None
            for k, v in tensors.items()
        }

    if inventory(state) != inventory(model.state_dict()):
        raise ValueError(f"{path}: saved tensor names, dtypes or shapes differ")
    model.load_state_dict(state, strict=True)
    model.eval()
    if model.seed != seed:
        raise ValueError(f"{path}: saved seed {model.seed} is not {seed}")
    if list(model.tasks) != list(tasks):
        raise ValueError(f"{path}: inventory {model.tasks} is not {tuple(tasks)}")
    return model, {"refused": CONFIG_REFUSAL, "differs": differs}


def _research_fallback(
    arm: str,
    path: Path,
    args: argparse.Namespace,
    expected: Any,
    manifest: Any,
) -> tuple[Any, dict[str, Any]]:
    """Wrap the fallback model for the normal reference serving path."""
    import dataclasses
    import time

    from stave import checkpoint_smoke as cs  # pyright: ignore[reportMissingImports]
    from stave import pretrained_coverage as pc  # pyright: ignore[reportMissingImports]
    from stave import pretrained_flat as pf  # pyright: ignore[reportMissingImports]
    from stave import pretrained_serving as rps  # pyright: ignore[reportMissingImports]

    start = time.perf_counter()
    tokenizer = pc.load_tokenizer(args.tokenizer_dir, args.tokenizer_receipt)
    tasks: Sequence[str] = manifest.tasks
    model, check = research_reference(
        arm, path, args.model_dir, expected,
        [RESEARCH_IDS.get(t, t) for t in tasks], manifest.seed,
        flat=pf, checkpoint=cs,
    )  # fmt: skip
    identity = {
        "arms": {arm: rps._artifact(path)},
        "backbone": dataclasses.asdict(expected),
        "tokenizer": {
            "dir": str(args.tokenizer_dir),
            "receipt": str(args.tokenizer_receipt),
        },
    }
    loaded = rps.Loaded(
        tokenizer,
        {arm: model},
        tuple(RESEARCH_IDS.get(t, t) for t in tasks),
        manifest.seed,
        identity,
        time.perf_counter() - start,
    )
    return loaded, check


def _sha256(path: Path) -> str:
    with path.open("rb") as f:
        return hashlib.file_digest(f, "sha256").hexdigest()


def _variant(
    arm: str, bundle_dir: Path, args: argparse.Namespace, package: Path
) -> dict[str, Any]:
    import torch

    # These imports require the reference project's environment.
    from stave import checkpoint_smoke as cs  # pyright: ignore[reportMissingImports]
    from stave import pretrained_serving as rps  # pyright: ignore[reportMissingImports]

    from stave_infer import bundle, serving

    research_model = args.run / "terminal" / arm / "model.pt"
    manifest = bundle.read_manifest(bundle_dir)
    result: dict[str, Any] = {
        "arm": arm,
        "bundle": manifest.id,
        "research_model": str(research_model),
        "research_model_sha256": _sha256(research_model),
        "bundle_model_sha256": _sha256(bundle_dir / manifest.model.file),
    }
    receipt = json.loads((args.run / "receipt.json").read_text())
    expected = cs.Expected(**receipt["expected"])
    config_check: dict[str, Any] = {"refused": None}
    try:
        research = rps.load(
            {arm: research_model},
            tokenizer_dir=args.tokenizer_dir,
            receipt=args.tokenizer_receipt,
            model_dir=args.model_dir,
            expected=expected,
            tasks=[RESEARCH_IDS.get(t, t) for t in manifest.tasks],
            seed=manifest.seed,
        )
    except Exception as exc:  # Keep the refusal in the comparison report.
        error = f"{type(exc).__name__}: {exc}"
        if arm != "B" or CONFIG_REFUSAL not in str(exc):
            result.update(ok=False, refused="research", error=error)
            return result
        try:
            research, config_check = _research_fallback(
                arm, research_model, args, expected, manifest
            )
        except Exception as fallback:
            result.update(
                ok=False,
                refused="research",
                error=error,
                fallback_error=f"{type(fallback).__name__}: {fallback}",
            )
            return result
        config_check["error"] = error
    result["research_config_check"] = config_check
    try:
        product = serving.load(bundle_dir, manifest, threads=None)
    except Exception as exc:
        result.update(ok=False, refused="product", error=f"{type(exc).__name__}: {exc}")
        return result
    states = compare_states(
        research.models[arm].state_dict(), product.model.state_dict()
    )
    research_report = rps.diagnose(
        research,
        sources(rps.Source, package, research=True),
        selector=[str(args.research_selector)],
        thresholds={RESEARCH_IDS.get(t, t): v for t, v in manifest.thresholds.items()},
        batch_size=manifest.batch_size,
    )
    product_report = serving.diagnose(
        product,
        sources(serving.Source, package),
        selector=[str(args.stave), "structural"],
    )
    logits = compare_logits(research_report["files"], product_report["files"], arm)
    rows_equal = product_records(research_report["files"]) == product_report["files"]
    errors = {
        "research": research_report["errors"],
        "product": product_report["errors"],
    }
    differs = config_check.get("differs", {})
    result.update(
        ok=result["research_model_sha256"]
        == manifest.provenance.get("source_model_sha256")
        and result["bundle_model_sha256"] == manifest.model.sha256
        and set(differs) <= {"init.sha256"}
        and states["equal"]
        and logits["equal"]
        and rows_equal
        and not any(errors.values()),
        states=states,
        logits=logits,
        rows_equal=rows_equal,
        errors=errors,
        threads={
            "intra_op": torch.get_num_threads(),
            "inter_op": torch.get_num_interop_threads(),
        },
    )
    del research, product
    gc.collect()
    return result


def main(argv: Sequence[str] | None = None) -> int:
    parser = argparse.ArgumentParser(prog="loader_parity.py", description=__doc__)
    parser.add_argument("--infer-src", type=Path, required=True)
    parser.add_argument("--run", type=Path, required=True)
    parser.add_argument("--model-dir", type=Path, required=True)
    parser.add_argument("--tokenizer-dir", type=Path, required=True)
    parser.add_argument("--tokenizer-receipt", type=Path, required=True)
    parser.add_argument("--bundle", action="append", required=True, metavar="ARM=DIR")
    parser.add_argument("--research-selector", type=Path, required=True)
    parser.add_argument("--stave", type=Path, required=True)
    parser.add_argument("--out", type=Path, required=True)
    args = parser.parse_args(argv)
    os.environ["HF_HUB_OFFLINE"] = "1"
    os.environ["TRANSFORMERS_OFFLINE"] = "1"
    sys.path.insert(0, str(args.infer_src.resolve()))
    import torch
    import transformers

    results = []
    with tempfile.TemporaryDirectory() as tmp:
        package = Path(tmp).resolve() / "fixture-pkg"
        shutil.copytree(REPO / "testdata" / "fixture-pkg", package)
        assert "testdata" not in package.parts
        for item in args.bundle:
            arm, _, directory = item.partition("=")
            result = _variant(arm, Path(directory), args, package)
            results.append(result)
            states, logits = result.get("states", {}), result.get("logits", {})
            print(
                f"{arm}: {'PASS' if result['ok'] else 'FAIL'} tensors={states.get('tensors')} "
                f"judged={logits.get('judged')} {result.get('error', '')}".rstrip(),
                flush=True,
            )
    report = {
        "python": sys.version.split()[0],
        "torch": torch.__version__,
        "transformers": transformers.__version__,
        "infer_src": str(args.infer_src.resolve()),
        "variants": results,
    }
    args.out.write_text(json.dumps(report, indent=2) + "\n")
    return 0 if all(r["ok"] for r in results) else 1


if __name__ == "__main__":
    sys.exit(main())
