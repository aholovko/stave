"""Variant B: a CAND cue with all-source pooling.

B uses token ids, CAND membership and source-token masks, excluding structural facts and other role memberships.
It can therefore admit projections that lack roles required by F/S. Its added parameters start from the same
seeded values as S, so the comparison changes inputs rather than initialization.

Loading ignores only `init.sha256`: it records the source checkpoint's training initialization, including the
original parameter names. Converted bundles preserve it, and PyTorch's normal draws differ across CPU architectures.
The saved seed, configuration and tensor inventory must still match, and the bundle hash pins the trained weights.
"""

import dataclasses
import hashlib
import json
from collections.abc import Sequence
from pathlib import Path
from typing import Any, cast

import torch
from torch import nn

from stave_infer import backbone as bb
from stave_infer import structural as sm
from stave_infer import structure as ps

FORMAT, KIND = "stave-flat-1", "flat-cand"
#: CAND membership selects the task's learned cue before encoding.
CUE = "cand-last-byte-row"
#: All slots use the F-arm pool while keeping the same head shape as F/S.
POOL = "all-source-tokens"
ModelError = sm.ModelError


# Admission and batches


def admit(p: ps.Projection) -> str | None:
    """Return why B cannot use `p`, or None if B admits it."""
    if p.status not in ("admitted", "incomplete"):
        return f"{p.status}: {p.reason}"
    if p.encoding is None or p.mu is None:
        return "no encoding or membership"
    tokens = len(p.encoding.tokens)
    if len(p.mu) != tokens or p.tokens != tokens:
        return (
            f"padded or inconsistent: {len(p.mu)} membership rows for {tokens} tokens"
        )
    roles = ps.ROLES.get(p.task)
    if roles is None or roles[0] != "CAND":
        return f"task {p.task!r} has no leading CAND role"
    if any(len(row) != len(roles) for row in p.mu):
        return f"membership rows are not {len(roles)} wide"
    if not any(row[0] for row in p.mu):
        return "CAND has no token"
    return None


@dataclasses.dataclass(frozen=True)
class FlatBatch:
    """One task-homogeneous right-padded microbatch of B."""

    task: str
    candidates: tuple[str, ...]
    ids: torch.Tensor
    cand: torch.Tensor
    masks: torch.Tensor
    valid: torch.Tensor
    attention: torch.Tensor
    positions: torch.Tensor


def collate(projections: Sequence[ps.Projection]) -> FlatBatch:
    """Pad projections of one task that B admits into one microbatch."""
    tasks = sorted({p.task for p in projections})
    if len(tasks) != 1:
        raise ModelError(f"a microbatch holds one task, not {tasks}")
    task, slots = tasks[0], len(ps.ROLES.get(tasks[0], ()))
    for p in projections:
        if (reason := admit(p)) is not None:
            raise ModelError(f"{p.candidate}: {reason}")
    encodings = [p.encoding for p in projections if p.encoding is not None]
    ids = bb.pad_batch([[t.id for t in e.tokens] for e in encodings])
    width = ids.shape[1]
    cand, masks = [], []
    for p, e in zip(projections, encodings, strict=True):
        assert p.mu is not None
        pad = width - len(e.tokens)
        cand.append([row[0] for row in p.mu] + [0] * pad)
        source = [t.end is not None for t in e.tokens] + [False] * pad
        masks.append(ps.additive_mask([source] * slots))
    valid, attention = bb.bidirectional_mask(ids)
    return FlatBatch(
        task,
        tuple(p.candidate for p in projections),
        ids,
        torch.tensor(cand, dtype=torch.float32),
        torch.tensor(masks, dtype=torch.float32),
        valid,
        attention,
        bb.position_ids(ids),
    )


# Model


class FlatModel(nn.Module):
    """The backbone with the CAND cue and one all-source head per task."""

    def __init__(self, backbone: Any, *, tasks: Sequence[str], seed: int) -> None:
        super().__init__()
        structural = sm.StructuralModel(backbone, tasks=tasks, seed=seed)
        self.backbone = backbone
        self.seed = seed
        self.tasks = structural.tasks
        self.inject = structural.inject
        # Insertion preserves task order; ParameterDict sorts keys when given a dict.
        self.cand = nn.ParameterDict()
        for t in self.tasks:
            row = cast(nn.Embedding, structural.roles[t]).weight[:1]
            self.cand[t] = nn.Parameter(row.detach().clone())
        self.heads = structural.heads
        self.init = {
            "from": sm.format_of(self.tasks),
            "seed": seed,
            "tasks": list(self.tasks),
            "sha256": digest(self),
        }
        #: Preserve the source initialization fingerprint across task renames and CPU architectures.
        self.saved_init_sha256: str | None = None

    def added(self) -> list[tuple[str, nn.Parameter]]:
        return [
            (n, p) for n, p in self.named_parameters() if not n.startswith("backbone.")
        ]

    def encode(self, batch: FlatBatch) -> Any:
        cue = batch.cand[..., None] @ self.cand[batch.task]
        embeds = self.backbone.embeddings.word_embeddings(batch.ids) + self.inject(cue)
        return self.backbone(
            inputs_embeds=embeds,
            attention_mask=batch.attention,
            position_ids=batch.positions,
            token_type_ids=torch.zeros_like(batch.ids),
            use_cache=False,
            output_attentions=False,
        )

    def forward(self, batch: FlatBatch) -> sm.Output:
        hidden = self.encode(batch).last_hidden_state
        return self.heads[batch.task](hidden, batch.cand[..., None], batch.masks)


def digest(model: FlatModel) -> str:
    """Hash the sorted names, dtypes, shapes and bytes of the added tensors with SHA-256."""
    h = hashlib.sha256()
    for name, p in sorted(model.added(), key=lambda item: item[0]):
        t = p.detach().cpu().contiguous()
        h.update(json.dumps([name, str(t.dtype), list(t.shape)]).encode())
        h.update(t.numpy().tobytes())
    return h.hexdigest()


# Serialization


def config_json(model: FlatModel, expected: bb.Expected) -> str:
    """Describe the exact architecture, backbone identity and initialization of `model` as JSON."""
    backbone = model.backbone.config
    config = {
        "format": FORMAT,
        "kind": KIND,
        "backbone": {
            "revision": expected.revision,
            "weight_bytes": expected.weight_bytes,
            "weight_sha256": expected.weight_sha256,
            "config_sha256": expected.config_sha256,
            "parameters": expected.parameters,
            "hidden": backbone.hidden_size,
            "layers": backbone.num_hidden_layers,
        },
        "tasks": list(model.tasks),
        "slots": {t: len(ps.ROLES[t]) for t in model.tasks},
        "cue": CUE,
        "pool": POOL,
        "widths": {"fact": sm.FACT_WIDTH, "head": sm.HEAD_WIDTH, "mlp": sm.MLP_WIDTH},
        "dropout": sm.DROPOUT,
        "init_std": sm.INIT_STD,
        "init": model.init,
        "added_parameters": sum(p.numel() for _, p in model.added()),
    }
    return json.dumps(config, sort_keys=True)


def save(model: FlatModel, path: Path, expected: bb.Expected) -> None:
    torch.save(
        {
            "format": FORMAT,
            "config_json": config_json(model, expected),
            "state": model.state_dict(),
        },
        path,
    )


def _saved_inventory(path: Path, saved: Any) -> tuple[list[str], int]:
    """Return the saved tasks and seed after the format and type checks."""
    if not isinstance(saved, dict) or saved.get("format") != FORMAT:
        found = saved.get("format") if isinstance(saved, dict) else type(saved)
        raise ModelError(f"{path}: format {found!r} is not {FORMAT}")
    if not isinstance(saved.get("state"), dict):
        raise ModelError(f"{path}: saved state is not a tensor mapping")
    text = saved.get("config_json")
    try:
        config = json.loads(text) if isinstance(text, str) else None
    except json.JSONDecodeError as exc:
        raise ModelError(f"{path}: saved config is not JSON") from exc
    if not isinstance(config, dict) or [config.get("format"), config.get("kind")] != [
        FORMAT,
        KIND,
    ]:
        raise ModelError(f"{path}: saved config is not a {FORMAT} {KIND} config")
    tasks, init = config.get("tasks"), config.get("init")
    if not isinstance(tasks, list) or not all(isinstance(t, str) for t in tasks):
        raise ModelError(f"{path}: saved config names no task inventory")
    if (
        not isinstance(init, dict)
        or type(init.get("seed")) is not int
        or init.get("tasks") != tasks
    ):
        raise ModelError(f"{path}: saved config has no valid init seed and tasks")
    return tasks, init["seed"]


def _without_init_digest(config: dict[str, Any]) -> tuple[dict[str, Any], Any]:
    """Separate the architecture-dependent initialization digest from the comparable configuration."""
    init = dict(config["init"])
    return {**config, "init": init}, init.pop("sha256", None)


def load(path: Path, backbone: Any, expected: bb.Expected) -> FlatModel:
    """Strict-load a saved B over `backbone` and return it in eval mode."""
    saved = torch.load(path, map_location="cpu", weights_only=True)
    tasks, seed = _saved_inventory(path, saved)
    model = FlatModel(backbone, tasks=tasks, seed=seed)
    found, saved_digest = _without_init_digest(json.loads(saved["config_json"]))
    rebuilt, _ = _without_init_digest(json.loads(config_json(model, expected)))
    if found != rebuilt or not isinstance(saved_digest, str):
        raise ModelError(f"{path}: saved config differs from the rebuilt model")
    model.saved_init_sha256 = saved_digest
    state = saved["state"]
    sm.check_inventory(path, state, model)
    model.load_state_dict(state, strict=True)
    return model.eval()
