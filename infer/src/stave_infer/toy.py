"""Deterministic test bundles with a real tokenizer and miniature random models.

The normal serializers and bundle layout exercise installation and serving without large trained weights.
Added parameters are redrawn so logits stay away from decision thresholds and comparisons remain stable.
These weights test the runtime, not prediction quality.

CLI: `python -m stave_infer.toy --variant S --out DIR --tokenizer-dir DIR`.
"""

import argparse
import hashlib
import json
import math
import shutil
import sys
from collections.abc import Sequence
from pathlib import Path

import torch

from stave_infer import backbone as bb
from stave_infer import bundle
from stave_infer import flat as fm
from stave_infer import structural as sm

#: Must match the tokenizer fixtures.
TOKENIZER_REVISION = "5604afdc964f6c53782a6813140ade5216b99006"
BACKBONE_REVISION = "toy"
#: Keeps fixture logits at least 0.07 from the threshold, with both decisions present, across all three variants.
SEED = 12
MODEL_FORMATS = {
    "B": "stave-flat-1",
    "F": "stave-structural-6",
    "S": "stave-structural-6",
}
BUDGETS = {"P1": 1023, "P2": 1023, "P3": 1023, "TF": 512, "DC": 512, "RP": 512}
LICENSE = (
    "Toy test bundle of STAVE, Apache-2.0; random weights, no predictive meaning.\n"
)
NOTICE = "Tokenizer files: microsoft/unixcoder-base (Apache-2.0).\n"


def _sha256(path: Path) -> str:
    with path.open("rb") as f:
        return hashlib.file_digest(f, "sha256").hexdigest()


def _config(vocab: int) -> dict[str, object]:
    return {
        "architectures": ["RobertaModel"],
        "model_type": "roberta",
        "vocab_size": vocab,
        "hidden_size": 16,
        "num_hidden_layers": 2,
        "num_attention_heads": 2,
        "intermediate_size": 32,
        "max_position_embeddings": 1026,
        "pad_token_id": 1,
        "type_vocab_size": 10,
        "layer_norm_eps": 1e-05,
        "hidden_act": "gelu",
    }


def _backbone_digest(model: torch.nn.Module) -> tuple[str, int]:
    """Return the SHA-256 and size of the initial tensors, a stand-in for the pretrained weight file."""
    h, size = hashlib.sha256(), 0
    for name, t in sorted(model.state_dict().items()):
        data = t.detach().contiguous().numpy().tobytes()
        h.update(name.encode() + b"\0" + data)
        size += len(data)
    return h.hexdigest(), size


def _redraw(model: torch.nn.Module, seed: int) -> None:
    generator = torch.Generator().manual_seed(seed)
    with torch.no_grad():
        for name, p in model.named_parameters():
            if name.startswith("backbone."):
                continue
            std = 0.1 if p.dim() == 1 else 1 / math.sqrt(p.shape[-1])
            p.copy_(torch.randn(p.shape, generator=generator) * std)


def make_toy_bundle(
    variant: str, out: Path, *, tokenizer_dir: Path, seed: int = SEED
) -> bundle.Manifest:
    """Write a toy bundle of `variant` to `out` and return its verified manifest."""
    if variant not in MODEL_FORMATS:
        raise ValueError(f"variant {variant!r} is not one of {sorted(MODEL_FORMATS)}")
    out.mkdir(parents=True, exist_ok=True)
    (out / "tokenizer").mkdir(exist_ok=True)
    for name in bundle.TOKENIZER_FILES:
        shutil.copyfile(tokenizer_dir / name, out / "tokenizer" / name)
    vocab = json.loads((tokenizer_dir / "config.json").read_text())["vocab_size"]
    config = out / "backbone-config.json"
    config.write_text(json.dumps(_config(vocab), indent=2, sort_keys=True) + "\n")
    with torch.random.fork_rng(devices=[]):
        torch.manual_seed(seed)
        backbone = bb.build(config, initialize=True)
    weight_sha256, weight_bytes = _backbone_digest(backbone)
    expected = bb.Expected(
        revision=BACKBONE_REVISION,
        weight_bytes=weight_bytes,
        weight_sha256=weight_sha256,
        config_sha256=_sha256(config),
        parameters=sum(p.numel() for p in backbone.parameters()),
    )
    model_path = out / "model.pt"
    if variant == "B":
        flat = fm.FlatModel(backbone, tasks=bundle.TASKS, seed=seed)
        _redraw(flat, seed)
        fm.save(flat, model_path, expected)
    else:
        structural = sm.StructuralModel(backbone, tasks=bundle.TASKS, seed=seed)
        _redraw(structural, seed)
        sm.save(structural, model_path, expected)
    (out / "LICENSE").write_text(LICENSE)
    (out / "NOTICE").write_text(NOTICE)
    manifest = {
        "format": bundle.FORMAT,
        "id": f"toy-{variant}",
        "variant": variant,
        "version": "0.0.0",
        "description": f"toy {variant} bundle for tests (seed {seed}); no predictive meaning",
        "tasks": list(bundle.TASKS),
        "seed": seed,
        "thresholds": {t: 0.0 for t in bundle.TASKS},
        "batch_size": 2,
        "input_budgets": BUDGETS,
        "model": {
            "file": "model.pt",
            "format": MODEL_FORMATS[variant],
            "sha256": _sha256(model_path),
            "bytes": model_path.stat().st_size,
        },
        "backbone": {
            "name": "stave-toy-roberta",
            "revision": expected.revision,
            "config": config.name,
            "config_sha256": expected.config_sha256,
            "weight_sha256": expected.weight_sha256,
            "weight_bytes": expected.weight_bytes,
            "parameters": expected.parameters,
        },
        "tokenizer": {
            "dir": "tokenizer",
            "revision": TOKENIZER_REVISION,
            "files": {
                name: _sha256(out / "tokenizer" / name)
                for name in bundle.TOKENIZER_FILES
            },
        },
        "provenance": {"generator": "stave_infer.toy", "seed": seed},
        "license": "Apache-2.0",
    }
    (out / bundle.MANIFEST).write_text(json.dumps(manifest, indent=2) + "\n")
    m = bundle.read_manifest(out)
    bundle.verify_files(out, m)
    return m


def main(argv: Sequence[str] | None = None) -> int:
    parser = argparse.ArgumentParser(
        prog="python -m stave_infer.toy",
        description="write a toy model bundle for tests",
    )
    parser.add_argument("--variant", required=True, choices=sorted(MODEL_FORMATS))
    parser.add_argument("--out", type=Path, required=True)
    parser.add_argument("--tokenizer-dir", type=Path, required=True)
    parser.add_argument("--seed", type=int, default=SEED)
    args = parser.parse_args(argv)
    try:
        m = make_toy_bundle(
            args.variant, args.out, tokenizer_dir=args.tokenizer_dir, seed=args.seed
        )
    except (ValueError, OSError) as exc:
        print(f"stave_infer.toy: {' '.join(str(exc).split())}", file=sys.stderr)
        return 1
    print(f"{m.id} {args.out}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
