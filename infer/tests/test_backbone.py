"""The RoBERTa backbone built from its configuration alone, and the batch tensors."""

import dataclasses
import json
from pathlib import Path

import pytest
import torch
from support import BACKBONE_CONFIG

from stave_infer import backbone

TOY_CONFIG = {
    "architectures": ["RobertaModel"],
    "model_type": "roberta",
    "vocab_size": 1024,
    "hidden_size": 16,
    "num_hidden_layers": 2,
    "num_attention_heads": 2,
    "intermediate_size": 32,
    "max_position_embeddings": 1026,
    "pad_token_id": 1,
    "type_vocab_size": 10,
    "use_cache": True,
}


def _toy(tmp_path: Path) -> Path:
    path = tmp_path / "backbone-config.json"
    path.write_text(json.dumps(TOY_CONFIG))
    return path


def test_build_yields_the_wrapper_semantics_from_the_config_alone(
    tmp_path: Path,
) -> None:
    model = backbone.build(_toy(tmp_path))

    assert type(model).__name__ == "RobertaModel" and not model.training
    assert model.config._attn_implementation == "eager"
    assert model.config.is_decoder is True and model.config.use_cache is False
    assert model.pooler is not None
    assert model.config.hidden_size == 16 and model.config.num_hidden_layers == 2
    assert all(p.dtype == torch.float32 for p in model.parameters())


def test_build_on_the_pinned_backbone_config_has_its_parameter_count() -> None:
    model = backbone.build(BACKBONE_CONFIG)

    assert sum(p.numel() for p in model.parameters()) == 125_929_728
    assert model.config.vocab_size == 51416 and model.config.hidden_size == 768


def test_build_leaves_the_weights_to_the_strict_load_that_follows(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    from transformers.models.roberta import modeling_roberta

    def refuse(*args: object, **kwargs: object) -> None:
        raise AssertionError("weights were initialized")

    monkeypatch.setattr(
        modeling_roberta.RobertaPreTrainedModel, "_init_weights", refuse
    )
    model = backbone.build(_toy(tmp_path))
    assert model.pooler is not None
    with pytest.raises(AssertionError, match="initialized"):
        backbone.build(_toy(tmp_path), initialize=True)


def test_build_refuses_a_config_that_is_not_roberta(tmp_path: Path) -> None:
    path = tmp_path / "c.json"
    path.write_text(json.dumps({**TOY_CONFIG, "model_type": "bert"}))
    with pytest.raises(ValueError, match="roberta"):
        backbone.build(path)


def test_expected_is_the_backbone_identity_config_json_records() -> None:
    names = [f.name for f in dataclasses.fields(backbone.Expected)]
    assert names == [
        "revision",
        "weight_bytes",
        "weight_sha256",
        "config_sha256",
        "parameters",
    ]


def test_pad_batch_right_pads_with_the_pad_id_and_masks_follow() -> None:
    ids = backbone.pad_batch([[0, 5, 6, 2], [0, 7, 2]])
    assert backbone.PAD_ID == 1
    assert ids.tolist() == [[0, 5, 6, 2], [0, 7, 2, 1]]
    valid, attention = backbone.bidirectional_mask(ids)
    assert valid.tolist() == [[True] * 4, [True, True, True, False]]
    assert attention.shape == (2, 4, 4)
    assert torch.equal(attention, valid[:, None, :] & valid[:, :, None])
    assert backbone.position_ids(ids).tolist() == [[2, 3, 4, 5], [2, 3, 4, 1]]
