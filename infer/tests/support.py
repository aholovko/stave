"""Shared test support: fixture paths, the toy tokenizer and the built `stave` binary."""

import hashlib
import importlib.util
import json
import os
import re
import shutil
import subprocess
import sys
import tempfile
import zlib
from pathlib import Path
from typing import Any

import pytest

REPO = Path(__file__).resolve().parents[2]
FIXTURES = Path(__file__).parent / "fixtures"
TOKENIZER_DIR = FIXTURES / "tokenizer"
BACKBONE_CONFIG = FIXTURES / "backbone-config.json"
FIXTURE_PKG = REPO / "testdata" / "fixture-pkg"
TOKENIZER_NAMES = (
    "vocab.json",
    "merges.txt",
    "tokenizer_config.json",
    "special_tokens_map.json",
    "config.json",
)


def sha256(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def tokenizer_files(directory: Path = TOKENIZER_DIR) -> dict[str, str]:
    """Return the SHA-256 of each of the five tokenizer files, as a manifest pins them."""
    return {name: sha256((directory / name).read_bytes()) for name in TOKENIZER_NAMES}


def copy_tokenizer(directory: Path) -> Path:
    """Copy only bundle inputs, excluding fixture documentation that the loader would reject."""
    directory.mkdir(parents=True, exist_ok=True)
    for name in TOKENIZER_NAMES:
        shutil.copyfile(TOKENIZER_DIR / name, directory / name)
    return directory


class ToyTokenizer:
    """Byte-level words with varied ids for model-input tests."""

    cls_token = "<s>"
    sep_token = "</s>"
    unk_token = "<unk>"
    _pattern = re.compile(rb"\s*\w+|\s*[^\w\s]+|\s+")

    def __init__(self) -> None:
        self.byte_decoder = {chr(0x100 + b): b for b in range(256)}
        self.added_tokens_encoder = {"<s>": 0, "<pad>": 1, "</s>": 2, "<unk>": 3}

    def tokenize(self, text: str) -> list[str]:
        return [
            "".join(chr(0x100 + b) for b in m.group())
            for m in self._pattern.finditer(text.encode("utf-8"))
        ]

    def convert_tokens_to_ids(self, tokens: list[str]) -> list[int]:
        return [
            self.added_tokens_encoder.get(t, 5 + zlib.crc32(t.encode()) % 1000)
            for t in tokens
        ]


_built: list[Path] = []


def go_stave() -> Path:
    """Build this checkout's CLI once per test session; skip when Go is unavailable."""
    if shutil.which("go") is None:
        pytest.skip("go toolchain not on PATH")
    if not _built:
        name = "stave.exe" if os.name == "nt" else "stave"
        binary = REPO / "build" / name
        subprocess.run(
            ["go", "build", "-o", str(binary), "./cmd/stave"], cwd=REPO, check=True
        )
        _built.append(binary)
    return _built[0]


def tool(relative: str) -> Any:
    """Import a repository tool script (`tools/...`) as a module."""
    path = REPO / "tools" / relative
    spec = importlib.util.spec_from_file_location(path.stem, path)
    assert spec is not None and spec.loader is not None, path
    module = importlib.util.module_from_spec(spec)
    sys.modules[path.stem] = module  # dataclasses resolve their module by name
    spec.loader.exec_module(module)
    return module


def selector() -> list[str]:
    return [str(go_stave()), "structural"]


SIX = ("P1", "P2", "P3", "TF", "DC", "RP")
#: Must match the saved configuration fixture.
REVISION = "5604afdc964f6c53782a6813140ade5216b99006"
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
}

#: Exercise empty optional THEN_LAST and required ARGS roles alongside complete candidates.
P1_SRC = (
    b"package p\n\nfunc F(a int) int {\n\tif a > 0 {\n\t\treturn 1\n\t} else {\n"
    b"\t\treturn 2\n\t}\n}\n\nfunc G(b int) int {\n\tif b > 1 {\n\t\tb++\n\t\treturn b\n"
    b"\t} else if b < 0 {\n\t\treturn -b\n\t} else {\n\t\treturn 0\n\t}\n}\n\n"
    b"func H(c bool) {\n\tif c {\n\t} else {\n\t\tprintln(c)\n\t}\n}\n"
)
P3_SRC = b"package p\n\nfunc F_a(p_x, q_y int, _ int) (r_z int) { return p_x + q_y }\n"
TF_SRC = (
    b'package p\n\nimport "testing"\n\nfunc TestA(t *testing.T) {\n'
    b'\tt.Error("")\n\tt.Errorf("got %d", 1)\n}\n'
)


def toy_backbone(seed: int = 7, *, initialize: bool = True, **overrides: Any) -> Any:
    """Use the production backbone builder with a small configuration.

    `initialize=False` exercises the path that leaves weights for a strict checkpoint load.
    """
    import torch

    from stave_infer import backbone

    with tempfile.TemporaryDirectory() as tmp:
        path = Path(tmp) / "backbone-config.json"
        path.write_text(json.dumps({**TOY_CONFIG, **overrides}))
        torch.manual_seed(seed)
        return backbone.build(path, initialize=initialize)


def expected_for(model: Any) -> Any:
    """Return a stand-in backbone identity whose parameter count is that of the model."""
    from stave_infer import backbone

    return backbone.Expected(
        revision=REVISION,
        weight_bytes=1,
        weight_sha256="0" * 64,
        config_sha256="1" * 64,
        parameters=sum(p.numel() for p in model.parameters()),
    )


def projections(task: str, source: bytes, tokenizer: Any = None) -> dict[str, Any]:
    """Project every selector result of `source` for `task` at the 1023 budget."""
    from stave_infer import structure

    tokenizer = tokenizer or ToyTokenizer()
    with tempfile.TemporaryDirectory() as tmp:
        command = selector()
        if task in ("P2", "P3", "TF"):
            command += ["--ledger", str(Path(tmp) / "ledger.json")]
        if task == "TF":
            command += ["--path", "p/x_test.go", "--sha256", sha256(source)]
        results = structure.run_selector(command, task, source, "x")
    return {
        r["cand"]: structure.project(tokenizer, source, r, max_tokens=1023)
        for r in results
    }


def copy_fixture_package(directory: Path) -> Path:
    """Copy fixtures outside `testdata` so the TF path gate permits them."""
    target = directory / "fixture-pkg"
    shutil.copytree(FIXTURE_PKG, target)
    assert "testdata" not in target.resolve().parts
    return target.resolve()
