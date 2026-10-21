"""Variant-specific loading, admission and collation.

B uses the flat model and can admit incomplete structural projections. F/S require every mandatory role.
Each loaded model must match the bundle's seed and task inventory.
"""

import pickle
from collections.abc import Sequence
from pathlib import Path
from typing import Any

from stave_infer import backbone as bb
from stave_infer import flat as fm
from stave_infer import structural as sm
from stave_infer import structure as ps

ARMS = ("B", "F", "S")


class VariantError(ValueError):
    """A model that does not load as the named variant, or inconsistent inputs."""


def load_model(
    arm: str,
    path: Path,
    backbone: Any,
    expected: bb.Expected,
    tasks: Sequence[str],
    seed: int,
) -> Any:
    """Strict-load a saved arm over `backbone` and check it against the seed and inventory."""
    if arm not in ARMS:
        raise VariantError(f"arm {arm!r} is not one of {ARMS}")
    parameters = sum(p.numel() for p in backbone.parameters())
    if parameters != expected.parameters:
        raise VariantError(
            f"backbone has {parameters} parameters, expected {expected.parameters}"
        )
    try:
        model = (fm.load if arm == "B" else sm.load)(path, backbone, expected)
    except (
        sm.ModelError,
        OSError,
        RuntimeError,
        EOFError,
        KeyError,
        TypeError,
        ValueError,
        pickle.UnpicklingError,
    ) as exc:
        raise VariantError(f"{path}: {' '.join(str(exc).split())}") from exc
    if model.seed != seed:
        raise VariantError(f"{path}: saved seed {model.seed} is not {seed}")
    if list(model.tasks) != list(tasks):
        raise VariantError(f"{path}: inventory {model.tasks} is not {tuple(tasks)}")
    return model


def collate(arm: str, projections: Sequence[ps.Projection]) -> Any:
    return fm.collate(projections) if arm == "B" else sm.collate(projections, arm)


def admit(arm: str, p: ps.Projection) -> str | None:
    """Return the documented withholding reason, or None after a check that the inputs collate."""
    reason = (
        fm.admit(p)
        if arm == "B"
        else (None if p.status == "admitted" else f"{p.status}: {p.reason}")
    )
    if reason is None:
        try:
            collate(arm, [p])
        except (ps.StructureError, sm.ModelError, KeyError) as exc:
            raise VariantError(
                f"{p.candidate}: inconsistent {arm} inputs: {exc}"
            ) from exc
    return reason
