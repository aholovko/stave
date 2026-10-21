"""Package a terminal training model as a `stave-model-bundle-1` zip.

The run must contain receipt.json, plan.json and terminal/<arm>/model.pt. Receipts pin the plan, model,
backbone configuration and five tokenizer files so packaging cannot silently mix artifacts.

Output is OUT/<id>/ and OUT/<id>.zip, with fixed zip metadata for reproducibility. Existing outputs are refused.
Legacy research task IDs are converted after receipt verification. `stave model add` checks model compatibility.
"""

import argparse
import dataclasses
import hashlib
import json
import pickle
import re
import shutil
import sys
import zipfile
from collections.abc import Sequence
from pathlib import Path
from typing import Any

from stave_infer import bundle

REPO = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(REPO / "tools"))
from bundle import checkpoint_ids  # noqa: E402

MODEL_FORMATS = {
    "B": "stave-flat-1",
    "F": "stave-structural-6",
    "S": "stave-structural-6",
}
BUDGETS = {"P1": 1023, "P2": 1023, "P3": 1023, "TF": 512, "DC": 512, "RP": 512}
LICENSE = "Apache-2.0"
ZIP_TIME = (1980, 1, 1, 0, 0, 0)


class BuildError(ValueError):
    """An input that does not match its receipt, or an output already present."""


@dataclasses.dataclass(frozen=True)
class Built:
    id: str
    directory: Path
    zip: Path
    size: int
    sha256: str


def _sha256(path: Path) -> str:
    with path.open("rb") as f:
        return hashlib.file_digest(f, "sha256").hexdigest()


def _json(path: Path) -> Any:
    try:
        return json.loads(path.read_bytes())
    except (OSError, ValueError) as exc:
        raise BuildError(f"{path}: {exc}") from exc


def _run_name(run: Path) -> str:
    """Use `<task>/<execution>` as provenance, omitting a trailing `run/` directory."""
    parts = run.resolve().parts
    return "/".join(parts[-3:-1] if parts[-1] == "run" else parts[-2:])


def _identity(run: Path, arm: str) -> tuple[dict[str, Any], dict[str, Any], str]:
    """Return the receipt and plan of a complete arm, with the SHA-256 of the plan."""
    receipt = _json(run / "receipt.json")
    try:
        status = receipt["arms"][arm]["status"]
        if receipt["status"] != "complete" or status != "complete":
            raise BuildError(
                f"{run / 'receipt.json'}: run {receipt['status']}, arm {arm} {status}; "
                "only a complete arm of a complete run is bundled"
            )
        pinned = receipt["hashes"]["plan.json"]
        receipt["hashes"][f"terminal/{arm}/model.pt"]
        receipt["expected"]
    except (KeyError, TypeError) as exc:
        raise BuildError(f"{run / 'receipt.json'}: no {exc} for arm {arm}") from exc
    plan_sha256 = _sha256(run / "plan.json")
    if plan_sha256 != pinned:
        raise BuildError(
            f"{run / 'plan.json'} has sha256 {plan_sha256}, not the receipt's {pinned}"
        )
    plan = _json(run / "plan.json")
    if arm not in plan.get("arms", ()):
        raise BuildError(
            f"{run / 'plan.json'}: arm {arm} is not among {plan.get('arms')}"
        )
    return receipt, plan, plan_sha256


def _tokenizer(
    directory: Path, receipt_path: Path
) -> tuple[dict[str, Any], dict[str, str]]:
    """Return the tokenizer receipt and the SHA-256 of each of the five files it pins."""
    receipt = _json(receipt_path)
    entries = {e["file"]: e for e in receipt.get("files", [])}
    files = {}
    for name in bundle.TOKENIZER_FILES:
        path = directory / name
        if name not in entries:
            raise BuildError(f"{receipt_path}: no entry for {name}")
        size, sha256 = path.stat().st_size, _sha256(path)
        if (size, sha256) != (entries[name]["bytes"], entries[name]["sha256"]):
            raise BuildError(
                f"{path} has {size} bytes and sha256 {sha256}, not the tokenizer "
                f"receipt's {entries[name]['bytes']} and {entries[name]['sha256']}"
            )
        files[name] = sha256
    return receipt, files


def _zip(directory: Path, archive: Path) -> None:
    """Fix entry order and metadata so identical inputs produce identical zip bytes."""
    partial = archive.with_name(f".{archive.name}.partial")
    with zipfile.ZipFile(partial, "w") as z:
        for path in sorted(p for p in directory.rglob("*") if p.is_file()):
            name = f"{directory.name}/{path.relative_to(directory).as_posix()}"
            info = zipfile.ZipInfo(name, date_time=ZIP_TIME)
            info.compress_type = zipfile.ZIP_DEFLATED
            info.external_attr = 0o100644 << 16
            with path.open("rb") as src, z.open(info, "w") as dst:
                shutil.copyfileobj(src, dst, 1 << 20)
    partial.replace(archive)


def _model(
    source: Path, destination: Path, pinned: str, tasks: list[str], arm: str
) -> bool:
    """Verify the research model before loading it; return whether task IDs were converted."""
    import torch

    source_sha256 = _sha256(source)
    if source_sha256 != pinned:
        raise BuildError(
            f"{source}: sha256 {source_sha256} is not the receipt's {pinned}"
        )
    try:
        saved = torch.load(source, map_location="cpu", weights_only=True)
        converted = checkpoint_ids.convert_checkpoint(saved)
        config = json.loads(saved["config_json"])
        if config["tasks"] != tasks:
            raise ValueError("checkpoint tasks do not match the plan")
        if saved["format"] != MODEL_FORMATS[arm]:
            raise ValueError("checkpoint format does not match the arm")
    except (
        ValueError,
        TypeError,
        KeyError,
        RuntimeError,
        pickle.UnpicklingError,
        zipfile.BadZipFile,
    ) as exc:
        raise BuildError(f"{source}: {exc}") from exc
    changed = checkpoint_ids.convert_tasks(tasks) != tasks
    if changed:
        # A fixed filename gives the PyTorch archive the same internal root on every build.
        torch.save(converted, destination)
    else:
        shutil.copyfile(source, destination)
    return changed


def build(
    *,
    run: Path,
    arm: str,
    ident: str,
    version: str,
    backbone_config: Path,
    tokenizer_dir: Path,
    tokenizer_receipt: Path,
    out: Path,
    description: str | None = None,
) -> Built:
    """Build the bundle `out/<ident>/` and its zip `out/<ident>.zip` from the `arm` of the run."""
    if arm not in MODEL_FORMATS:
        raise BuildError(f"arm {arm!r} is not one of {sorted(MODEL_FORMATS)}")
    if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9._-]*", ident):
        raise BuildError(
            f"id {ident!r} is not a name of letters, digits, '.', '_' and '-'"
        )
    receipt, plan, plan_sha256 = _identity(run, arm)
    try:
        tasks = checkpoint_ids.convert_tasks(plan.get("tasks"))
        if tasks != list(bundle.TASKS):
            raise ValueError("a bundle requires all six tasks")
        if not isinstance(plan.get("thresholds"), dict) or set(
            plan["thresholds"]
        ) != set(plan["tasks"]):
            raise ValueError("threshold keys do not match the plan tasks")
    except ValueError as exc:
        raise BuildError(f"{run / 'plan.json'}: {exc}") from exc
    expected = receipt["expected"]
    config_sha256 = _sha256(backbone_config)
    if config_sha256 != expected.get("config_sha256"):
        raise BuildError(
            f"{backbone_config} has sha256 {config_sha256}, not the receipt's expected "
            f"config_sha256 {expected.get('config_sha256')}"
        )
    tokens, tokenizer_files = _tokenizer(tokenizer_dir, tokenizer_receipt)
    directory, archive = out / ident, out / f"{ident}.zip"
    for path in (directory, archive):
        if path.exists():
            raise BuildError(f"{path} exists; a bundle is never overwritten")

    directory.mkdir(parents=True)
    try:
        model = directory / "model.pt"
        source_hash = receipt["hashes"][f"terminal/{arm}/model.pt"]
        converted = _model(
            run / "terminal" / arm / "model.pt", model, source_hash, plan["tasks"], arm
        )
        model_sha256 = _sha256(model)
        shutil.copyfile(backbone_config, directory / "backbone-config.json")
        (directory / "tokenizer").mkdir()
        for name in bundle.TOKENIZER_FILES:
            shutil.copyfile(tokenizer_dir / name, directory / "tokenizer" / name)
        for name in ("LICENSE", "NOTICE"):
            shutil.copyfile(REPO / name, directory / name)
        name = _run_name(run)
        provenance: dict[str, Any] = {"run": name, "plan_sha256": plan_sha256}
        if converted:
            provenance.update(
                source_model_sha256=source_hash,
                source_tasks=plan["tasks"],
                conversion=checkpoint_ids.CONVERSION,
            )
            if arm == "B":
                provenance["init_sha256_note"] = (
                    "Original source initialization fingerprint, including E task tensor names."
                )
        manifest = {
            "format": bundle.FORMAT,
            "id": ident,
            "variant": arm,
            "version": version,
            "description": description
            or f"STAVE variant {arm}: the terminal model of training run {name}",
            "tasks": tasks,
            "seed": plan["init_seed"],
            "thresholds": {
                checkpoint_ids.product_task(t): plan["thresholds"][t]
                for t in plan["tasks"]
            },
            "batch_size": plan["eval_batch_size"],
            "input_budgets": BUDGETS,
            "model": {
                "file": model.name,
                "format": MODEL_FORMATS[arm],
                "sha256": model_sha256,
                "bytes": model.stat().st_size,
            },
            "backbone": {
                "name": tokens["model"],
                "revision": tokens["revision"],
                "config": "backbone-config.json",
                "config_sha256": config_sha256,
                "weight_sha256": expected["weight_sha256"],
                "weight_bytes": expected["weight_bytes"],
                "parameters": expected["parameters"],
            },
            "tokenizer": {
                "dir": "tokenizer",
                "revision": tokens["revision"],
                "files": tokenizer_files,
            },
            "provenance": provenance,
            "license": LICENSE,
        }
        (directory / bundle.MANIFEST).write_text(json.dumps(manifest, indent=2) + "\n")
        bundle.verify_files(directory, bundle.read_manifest(directory))
    except (KeyError, TypeError) as exc:
        shutil.rmtree(directory)
        raise BuildError(f"{run}: the plan or receipt has no {exc}") from exc
    except BaseException:
        shutil.rmtree(directory)
        raise
    _zip(directory, archive)
    return Built(ident, directory, archive, archive.stat().st_size, _sha256(archive))


def main(argv: Sequence[str] | None = None) -> int:
    parser = argparse.ArgumentParser(
        prog="make_bundle.py", description="build a STAVE model bundle zip"
    )
    parser.add_argument("--run", type=Path, required=True)
    parser.add_argument("--arm", required=True, choices=sorted(MODEL_FORMATS))
    parser.add_argument("--id", required=True)
    parser.add_argument("--version", required=True)
    parser.add_argument("--backbone-config", type=Path, required=True)
    parser.add_argument("--tokenizer-dir", type=Path, required=True)
    parser.add_argument("--tokenizer-receipt", type=Path, required=True)
    parser.add_argument("--out", type=Path, required=True)
    parser.add_argument("--description")
    args = parser.parse_args(argv)
    try:
        built = build(
            run=args.run,
            arm=args.arm,
            ident=args.id,
            version=args.version,
            backbone_config=args.backbone_config,
            tokenizer_dir=args.tokenizer_dir,
            tokenizer_receipt=args.tokenizer_receipt,
            out=args.out,
            description=args.description,
        )
    except (ValueError, OSError) as exc:
        print(f"make_bundle: {' '.join(str(exc).split())}", file=sys.stderr)
        return 1
    print(built.id, built.zip, built.size, built.sha256)
    return 0


if __name__ == "__main__":
    sys.exit(main())
