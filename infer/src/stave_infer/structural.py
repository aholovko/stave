"""F/S models with fact injection and role pooling.

Projected fact and role embeddings are added to word embeddings before the backbone, leaving its position embeddings,
normalization and dropout to run exactly once. Positions come from token ids because `inputs_embeds` cannot
identify padding. Empty role pools use learned vectors. F and S differ only in their pool masks.

Initialization touches only added parameters; checkpoint loading restores the full model. Task order,
initialization order, role inventories and saved format are part of the checkpoint contract.
"""

import dataclasses
import json
import math
from collections.abc import Sequence
from pathlib import Path
from typing import Any, NamedTuple, cast

import torch
from torch import nn

from stave_infer import backbone as bb
from stave_infer import structure as ps

FACT_WIDTH, HEAD_WIDTH, MLP_WIDTH = 64, 128, 256
DROPOUT, INIT_STD = 0.1, 0.02
FACT_NAMES = (
    "kind",
    "parent",
    "slot",
    "ordinal",
    "last",
    "depth",
    "literal",
    "segment",
)
TASKS = ("P1", "P2", "P3", "TF", "DC", "RP")
FORMATS = (
    "stave-structural-2",
    "stave-structural-3",
    "stave-structural-4",
    "stave-structural-6",
)


class ModelError(ValueError):
    """A batch, configuration or saved-state contract failure."""


def check_tasks(tasks: Sequence[str]) -> tuple[str, ...]:
    """Check that `tasks` is a supported inventory: a nonempty canonical subsequence of the six tasks."""
    tasks = tuple(tasks)
    if not tasks or tasks != tuple(t for t in TASKS if t in tasks):
        raise ModelError(f"tasks {tasks} are not a canonical subsequence of {TASKS}")
    return tasks


def format_of(tasks: Sequence[str]) -> str:
    """Select the checkpoint format required by the task inventory."""
    if "DC" in tasks or "RP" in tasks:
        return "stave-structural-6"
    if "TF" in tasks:
        return "stave-structural-4"
    if "P2" in tasks or "P3" in tasks:
        return "stave-structural-3"
    return "stave-structural-2"


# Batches


@dataclasses.dataclass(frozen=True)
class Batch:
    """One task-homogeneous right-padded microbatch of one arm."""

    task: str
    ids: torch.Tensor
    facts: torch.Tensor
    mu: torch.Tensor
    masks: torch.Tensor
    valid: torch.Tensor
    attention: torch.Tensor
    positions: torch.Tensor


def collate(projections: Sequence[ps.Projection], arm: str) -> Batch:
    """Pad admitted projections of one task into one microbatch of `arm`."""
    tasks = sorted({p.task for p in projections})
    if len(tasks) != 1:
        raise ModelError(f"a microbatch holds one task, not {tasks}")
    task, roles = tasks[0], len(ps.ROLES[tasks[0]])
    inputs = [p.arm(arm) for p in projections]
    sequences = []
    for p in projections:
        assert p.encoding is not None
        sequences.append([t.id for t in p.encoding.tokens])
    ids = bb.pad_batch(sequences)
    width = ids.shape[1]
    for p, (facts, _, _), sequence in zip(projections, inputs, sequences, strict=True):
        if len(facts) != len(sequence):
            raise ModelError(
                f"{p.candidate}: {len(facts)} fact rows for {len(sequence)} ids"
            )
    pad = width - torch.tensor([len(s) for s in sequences])
    facts = torch.tensor(
        [f + [list(ps.PAD)] * int(n) for (f, _, _), n in zip(inputs, pad, strict=True)]
    )
    mu = torch.tensor(
        [m + [[0] * roles] * int(n) for (_, m, _), n in zip(inputs, pad, strict=True)],
        dtype=torch.float32,
    )
    masks = torch.tensor(
        [
            [row + [-math.inf] * int(n) for row in mask]
            for (_, _, mask), n in zip(inputs, pad, strict=True)
        ],
        dtype=torch.float32,
    )
    valid, attention = bb.bidirectional_mask(ids)
    return Batch(task, ids, facts, mu, masks, valid, attention, bb.position_ids(ids))


# Model modules


class Output(NamedTuple):
    logits: torch.Tensor
    pooled: torch.Tensor
    weights: torch.Tensor


class RoleHead(nn.Module):
    """A CAND-mean query, masked attention pooling per role and a GELU MLP."""

    def __init__(self, hidden: int, roles: int) -> None:
        super().__init__()
        self.query = nn.Linear(hidden, HEAD_WIDTH, bias=False)
        self.key = nn.Linear(hidden, HEAD_WIDTH, bias=False)
        self.values = nn.Parameter(torch.empty(roles, HEAD_WIDTH, hidden))
        self.role_bias = nn.Parameter(torch.empty(roles, HEAD_WIDTH))
        self.empty = nn.Parameter(torch.empty(roles, HEAD_WIDTH))
        self.hidden = nn.Linear(roles * HEAD_WIDTH, MLP_WIDTH)
        self.out = nn.Linear(MLP_WIDTH, 1)
        self.dropout = nn.Dropout(DROPOUT)
        self.gelu = nn.GELU()

    def forward(
        self, hidden: torch.Tensor, mu: torch.Tensor, masks: torch.Tensor
    ) -> Output:
        cand = mu[..., :1]
        c = (hidden * cand).sum(1) / cand.sum(1)
        q = self.query(c)[:, None, :] + self.role_bias
        scores = torch.einsum("brd,bld->brl", q, self.key(hidden))
        scores = scores / math.sqrt(HEAD_WIDTH) + masks
        empty = torch.isneginf(masks).all(-1)[..., None]
        scores = torch.where(empty, torch.zeros_like(scores), scores)
        weights = torch.softmax(scores, -1).masked_fill(empty, 0.0)
        pooled = torch.einsum("brl,blh->brh", weights, hidden)
        values = torch.einsum("brh,rdh->brd", pooled, self.values)
        u = torch.where(empty, self.empty.expand_as(values), values)
        z = self.dropout(u.flatten(1))
        logits = self.out(self.gelu(self.hidden(z))).squeeze(-1)
        return Output(logits, u, weights)


class StructuralModel(nn.Module):
    """The backbone with fact injection and one head per task."""

    def __init__(self, backbone: Any, *, tasks: Sequence[str], seed: int) -> None:
        super().__init__()
        self.backbone = backbone
        self.seed = seed
        self.tasks = check_tasks(tasks)
        hidden = backbone.config.hidden_size
        # Module constructors must not consume the caller's random state.
        with torch.random.fork_rng(devices=[]):
            self.facts = nn.ModuleDict(
                {n: nn.Embedding(b, FACT_WIDTH) for n, b in zip(FACT_NAMES, ps.BUCKETS)}
            )
            self.roles = nn.ModuleDict(
                {t: nn.Embedding(len(ps.ROLES[t]), FACT_WIDTH) for t in self.tasks}
            )
            self.inject = nn.Linear(FACT_WIDTH, hidden, bias=False)
            self.heads = nn.ModuleDict(
                {t: RoleHead(hidden, len(ps.ROLES[t])) for t in self.tasks}
            )
        generator = torch.Generator().manual_seed(seed)
        params = dict(self.added())
        with torch.no_grad():
            for name in init_order(self):
                p = params[name]
                if name.endswith("bias") or name == "inject.weight":
                    p.zero_()
                else:
                    p.normal_(0.0, INIT_STD, generator=generator)

    def added(self) -> list[tuple[str, nn.Parameter]]:
        """Return the added parameters in declaration order."""
        return [
            (n, p) for n, p in self.named_parameters() if not n.startswith("backbone.")
        ]

    def encode(self, batch: Batch) -> Any:
        f = batch.mu @ cast(nn.Embedding, self.roles[batch.task]).weight
        for column, table in enumerate(self.facts.values()):
            f = f + table(batch.facts[..., column])
        embeds = self.backbone.embeddings.word_embeddings(batch.ids) + self.inject(f)
        return self.backbone(
            inputs_embeds=embeds,
            attention_mask=batch.attention,
            position_ids=batch.positions,
            token_type_ids=torch.zeros_like(batch.ids),
            use_cache=False,
            output_attentions=False,
        )

    def forward(self, batch: Batch) -> Output:
        hidden = self.encode(batch).last_hidden_state
        return self.heads[batch.task](hidden, batch.mu, batch.masks)


def init_order(model: StructuralModel) -> list[str]:
    """Keep existing tasks' seeded parameters stable when adding later tasks."""
    names = [n for n, _ in model.added()]

    def of(kind: str, task: str) -> list[str]:
        return [n for n in names if n.startswith(f"{kind}.{task}.")]

    blocks = [
        n for t in model.tasks if t != "P1" for n in of("roles", t) + of("heads", t)
    ]
    return [n for n in names if n not in blocks] + blocks


# Serialization


def config_json(model: StructuralModel, expected: bb.Expected) -> str:
    """Describe the exact architecture, backbone identity and initialization of `model` as JSON."""
    backbone = model.backbone.config
    config = {
        "format": format_of(model.tasks),
        "backbone": {
            "revision": expected.revision,
            "weight_bytes": expected.weight_bytes,
            "weight_sha256": expected.weight_sha256,
            "config_sha256": expected.config_sha256,
            "parameters": expected.parameters,
            "hidden": backbone.hidden_size,
            "layers": backbone.num_hidden_layers,
        },
        "facts": dict(zip(FACT_NAMES, ps.BUCKETS)),
        "tasks": list(model.tasks),
        "init_order": init_order(model),
        "roles": {t: list(ps.ROLES[t]) for t in model.tasks},
        "widths": {"fact": FACT_WIDTH, "head": HEAD_WIDTH, "mlp": MLP_WIDTH},
        "dropout": DROPOUT,
        "init_std": INIT_STD,
        "seed": model.seed,
        "added_parameters": sum(p.numel() for _, p in model.added()),
    }
    return json.dumps(config, sort_keys=True)


def save(model: StructuralModel, path: Path, expected: bb.Expected) -> None:
    torch.save(
        {
            "format": format_of(model.tasks),
            "config_json": config_json(model, expected),
            "state": model.state_dict(),
        },
        path,
    )


def _inventory(tensors: Any) -> dict[str, Any]:
    return {
        k: (v.dtype, tuple(v.shape)) if isinstance(v, torch.Tensor) else None
        for k, v in tensors.items()
    }


def check_inventory(path: Path, state: Any, model: nn.Module) -> None:
    """Reject dtype changes as well as names and shapes; a strict PyTorch load would cast them silently."""
    found, rebuilt = _inventory(state), _inventory(model.state_dict())
    if found != rebuilt:
        names = sorted(
            k for k in found.keys() | rebuilt.keys() if found.get(k) != rebuilt.get(k)
        )
        raise ModelError(
            f"{path}: saved tensor names, dtypes or shapes differ: {', '.join(names)}"
        )


def load(path: Path, backbone: Any, expected: bb.Expected) -> StructuralModel:
    """Strict-load a saved state over `backbone` and return the model in eval mode."""
    saved = torch.load(path, map_location="cpu", weights_only=True)
    if not isinstance(saved, dict) or saved.get("format") not in FORMATS:
        found = saved.get("format") if isinstance(saved, dict) else type(saved)
        raise ModelError(f"{path}: format {found!r} is not one of {FORMATS}")
    config = json.loads(saved["config_json"])
    tasks = config.get("tasks")
    if not isinstance(tasks, list):
        raise ModelError(f"{path}: saved config names no task inventory")
    seed = config.get("seed")
    if type(seed) is not int:
        raise ModelError(f"{path}: saved config seed {seed!r} is not an integer")
    if format_of(check_tasks(tasks)) != saved["format"]:
        raise ModelError(
            f"{path}: format {saved['format']!r} does not pair with tasks {tasks}"
        )
    model = StructuralModel(backbone, tasks=tasks, seed=seed)
    if config_json(model, expected) != saved["config_json"]:
        raise ModelError(f"{path}: saved config differs from the rebuilt model")
    check_inventory(path, saved["state"], model)
    model.load_state_dict(saved["state"], strict=True)
    return model.eval()
