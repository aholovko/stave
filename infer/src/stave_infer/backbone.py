"""RoBERTa construction and batch tensors for the saved-model contract.

The fine-tuned state includes every backbone tensor, so serving needs only the configuration, not a second
pretrained weight file. Decoder mode, eager bidirectional attention, positions and padding must match the
checkpoint's encoding rules.
"""

import dataclasses
from collections.abc import Sequence
from pathlib import Path
from typing import Any

import torch

PAD_ID = 1


@dataclasses.dataclass(frozen=True)
class Expected:
    """The backbone identity a saved model's configuration must name."""

    revision: str
    weight_bytes: int
    weight_sha256: str
    config_sha256: str
    parameters: int


def build(config_path: Path, *, initialize: bool = False) -> Any:
    """Build the CPU backbone, leaving weights for the strict checkpoint load.

    `initialize=True` supplies random weights for toy models.
    """
    from transformers import RobertaConfig, RobertaModel
    from transformers.modeling_utils import no_init_weights

    cfg = RobertaConfig.from_json_file(str(config_path))
    if cfg.model_type != "roberta":
        raise ValueError(f"{config_path}: model type {cfg.model_type!r} is not roberta")
    cfg.is_decoder = True
    cfg.use_cache = False
    cfg._attn_implementation = "eager"
    model: Any
    if initialize:
        model = RobertaModel(cfg, add_pooling_layer=True)
    else:
        with no_init_weights():
            model = RobertaModel(cfg, add_pooling_layer=True)
    if model.config._attn_implementation != "eager":
        raise ValueError(f"{config_path}: attention is not the eager implementation")
    return model.eval()


def pad_batch(sequences: Sequence[Sequence[int]]) -> torch.Tensor:
    """Pad the sequences on the right with the pad id."""
    width = max(len(s) for s in sequences)
    return torch.tensor([list(s) + [PAD_ID] * (width - len(s)) for s in sequences])


def bidirectional_mask(ids: torch.Tensor) -> tuple[torch.Tensor, torch.Tensor]:
    """Return the valid-token mask and its [B, L, L] product, which keeps attention bidirectional in decoder mode."""
    valid = ids.ne(PAD_ID)
    return valid, valid.unsqueeze(1) * valid.unsqueeze(2)


def position_ids(ids: torch.Tensor) -> torch.Tensor:
    """Match RoBERTa's padding-aware positions when supplying `inputs_embeds`."""
    valid = ids.ne(PAD_ID).long()
    return torch.cumsum(valid, 1) * valid + PAD_ID
