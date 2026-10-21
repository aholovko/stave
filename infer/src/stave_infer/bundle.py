"""Validate the `stave-model-bundle-1` manifest and its pinned files.

Manifest checks define the supported formats, tasks and token budgets. Hash checks bind the model, backbone
configuration and tokenizer to that manifest. Errors name the affected field for installation diagnostics;
model loading performs the remaining compatibility checks.
"""

import dataclasses
import hashlib
import json
import math
import re
from collections.abc import Mapping
from pathlib import Path, PurePosixPath
from typing import Any, NoReturn

FORMAT = "stave-model-bundle-1"
MANIFEST = "manifest.json"
SUPPORTED_MODEL_FORMATS: dict[str, tuple[str, ...]] = {
    "stave-flat-1": ("B",),
    "stave-structural-6": ("F", "S"),
}
TASKS = ("P1", "P2", "P3", "TF", "DC", "RP")
MAX_BUDGET = 1023
TOKENIZER_FILES = (
    "vocab.json",
    "merges.txt",
    "tokenizer_config.json",
    "special_tokens_map.json",
    "config.json",
)
_ID = re.compile(r"[A-Za-z0-9][A-Za-z0-9._-]*")
_TOP = frozenset(
    {
        "format", "id", "variant", "version", "description", "tasks", "seed",
        "thresholds", "batch_size", "input_budgets", "model", "backbone",
        "tokenizer", "provenance", "license",
    }
)  # fmt: skip
_MODEL = frozenset({"file", "format", "sha256", "bytes"})
_BACKBONE = frozenset(
    {
        "name", "revision", "config", "config_sha256", "weight_sha256",
        "weight_bytes", "parameters",
    }
)  # fmt: skip
_TOKENIZER = frozenset({"dir", "revision", "files"})


class BundleError(ValueError):
    """A manifest or bundle file that this runtime refuses. The message names the field."""


@dataclasses.dataclass(frozen=True)
class ModelFile:
    file: str
    format: str
    sha256: str
    bytes: int


@dataclasses.dataclass(frozen=True)
class Backbone:
    name: str
    revision: str
    config: str
    config_sha256: str
    weight_sha256: str
    weight_bytes: int
    parameters: int


@dataclasses.dataclass(frozen=True)
class Tokenizer:
    dir: str
    revision: str
    files: Mapping[str, str]


@dataclasses.dataclass(frozen=True)
class Manifest:
    format: str
    id: str
    variant: str
    version: str
    description: str
    tasks: tuple[str, ...]
    seed: int
    thresholds: Mapping[str, float]
    batch_size: int
    input_budgets: Mapping[str, int]
    model: ModelFile
    backbone: Backbone
    tokenizer: Tokenizer
    provenance: Mapping[str, Any]
    license: str


def _err(field: str, msg: str) -> NoReturn:
    raise BundleError(f"manifest {field}: {msg}")


def _object(value: Any, field: str, keys: frozenset[str]) -> dict[str, Any]:
    if not isinstance(value, dict):
        _err(field, "is not a JSON object")
    unknown = sorted(set(value) - keys)
    if unknown:
        _err(field, f"has unknown field {unknown[0]!r}")
    missing = sorted(keys - set(value))
    if missing:
        _err(field, f"is missing field {missing[0]!r}")
    return value


def _string(value: Any, field: str) -> str:
    if not isinstance(value, str) or not value:
        _err(field, f"{value!r} is not a nonempty string")
    return value


def _int(value: Any, field: str, minimum: int) -> int:
    if type(value) is not int or value < minimum:
        _err(field, f"{value!r} is not an integer of at least {minimum}")
    return value


def _sha(value: Any, field: str) -> str:
    if not (
        isinstance(value, str)
        and len(value) == 64
        and all(c in "0123456789abcdef" for c in value)
    ):
        _err(field, f"{value!r} is not 64 lowercase hex characters")
    return value


def _relative(value: Any, field: str) -> str:
    """Check that `value` is a slash-separated path that stays inside the bundle directory."""
    name = _string(value, field)
    parts = PurePosixPath(name).parts
    if (
        name.startswith("/")
        or "\\" in name
        or ":" in name
        or any(p in (".", "..") for p in parts)
    ):
        _err(field, f"{name!r} is not a relative path inside the bundle")
    return name


def _per_task(value: Any, field: str) -> dict[str, Any]:
    if not isinstance(value, dict) or set(value) != set(TASKS):
        _err(field, f"{value!r} does not name exactly the tasks {list(TASKS)}")
    return value


def _parse(document: Any) -> Manifest:
    d = _object(document, "document", _TOP)
    if d["format"] != FORMAT:
        _err("format", f"{d['format']!r} is not {FORMAT}")
    ident = _string(d["id"], "id")
    if not _ID.fullmatch(ident):
        _err("id", f"{ident!r} is not a name of letters, digits, '.', '_' and '-'")
    model = _object(d["model"], "model", _MODEL)
    model_format = model["format"]
    if model_format not in SUPPORTED_MODEL_FORMATS:
        _err(
            "model.format",
            f"{model_format!r} is not one of {sorted(SUPPORTED_MODEL_FORMATS)}; a new "
            "model format needs a new STAVE runtime release",
        )
    variants = SUPPORTED_MODEL_FORMATS[model_format]
    if d["variant"] not in variants:
        _err("variant", f"{d['variant']!r} is not one of {variants} for {model_format}")
    if d["tasks"] != list(TASKS):
        _err("tasks", f"{d['tasks']!r} are not the six tasks {list(TASKS)} in order")
    thresholds = _per_task(d["thresholds"], "thresholds")
    for task, value in thresholds.items():
        if type(value) not in (int, float) or not math.isfinite(value):
            _err("thresholds", f"{task}={value!r} is not a finite number")
    budgets = _per_task(d["input_budgets"], "input_budgets")
    for task, value in budgets.items():
        if type(value) is not int or not 1 <= value <= MAX_BUDGET:
            _err(
                "input_budgets",
                f"{task}={value!r} is not an integer from 1 to {MAX_BUDGET}",
            )
    backbone = _object(d["backbone"], "backbone", _BACKBONE)
    tokenizer = _object(d["tokenizer"], "tokenizer", _TOKENIZER)
    files = tokenizer["files"]
    if not isinstance(files, dict) or set(files) != set(TOKENIZER_FILES):
        found = sorted(files) if isinstance(files, dict) else files
        _err("tokenizer.files", f"{found!r} are not the files {list(TOKENIZER_FILES)}")
    if not isinstance(d["provenance"], dict):
        _err("provenance", "is not a JSON object")
    if not isinstance(d["description"], str):
        _err("description", "is not a string")
    return Manifest(
        format=FORMAT,
        id=ident,
        variant=d["variant"],
        version=_string(d["version"], "version"),
        description=d["description"],
        tasks=TASKS,
        seed=_int(d["seed"], "seed", 0),
        thresholds={t: float(thresholds[t]) for t in TASKS},
        batch_size=_int(d["batch_size"], "batch_size", 1),
        input_budgets={t: budgets[t] for t in TASKS},
        model=ModelFile(
            file=_relative(model["file"], "model.file"),
            format=model_format,
            sha256=_sha(model["sha256"], "model.sha256"),
            bytes=_int(model["bytes"], "model.bytes", 1),
        ),
        backbone=Backbone(
            name=_string(backbone["name"], "backbone.name"),
            revision=_string(backbone["revision"], "backbone.revision"),
            config=_relative(backbone["config"], "backbone.config"),
            config_sha256=_sha(backbone["config_sha256"], "backbone.config_sha256"),
            weight_sha256=_sha(backbone["weight_sha256"], "backbone.weight_sha256"),
            weight_bytes=_int(backbone["weight_bytes"], "backbone.weight_bytes", 1),
            parameters=_int(backbone["parameters"], "backbone.parameters", 1),
        ),
        tokenizer=Tokenizer(
            dir=_relative(tokenizer["dir"], "tokenizer.dir"),
            revision=_string(tokenizer["revision"], "tokenizer.revision"),
            files={
                name: _sha(files[name], f"tokenizer.files[{name!r}]")
                for name in TOKENIZER_FILES
            },
        ),
        provenance=d["provenance"],
        license=_string(d["license"], "license"),
    )


def read_manifest(directory: Path) -> Manifest:
    """Read the manifest of the bundle, or raise BundleError naming the first field this runtime refuses."""
    path = directory / MANIFEST
    try:
        document = json.loads(path.read_bytes())
    except OSError as exc:
        raise BundleError(f"manifest {path}: unreadable: {exc}") from exc
    except ValueError as exc:
        raise BundleError(f"manifest {path}: not JSON: {exc}") from exc
    return _parse(document)


def _digest(path: Path) -> str:
    with path.open("rb") as f:
        return hashlib.file_digest(f, "sha256").hexdigest()


def _check(path: Path, sha256: str, field: str, size: int | None = None) -> None:
    if not path.is_file():
        _err(field, f"{path} is missing")
    actual_size = path.stat().st_size
    if size is not None and actual_size != size:
        _err(
            field.replace(".sha256", ".bytes"),
            f"{path} has {actual_size} bytes, not {size}",
        )
    actual = _digest(path)
    if actual != sha256:
        _err(field, f"{path} has sha256 {actual}, not {sha256}")


def verify_files(directory: Path, m: Manifest) -> None:
    """Check that every pinned file exists with the size and SHA-256 of the manifest."""
    model = directory / m.model.file
    if not model.is_file():
        _err("model.file", f"{model} is missing")
    _check(model, m.model.sha256, "model.sha256", m.model.bytes)
    _check(
        directory / m.backbone.config,
        m.backbone.config_sha256,
        "backbone.config_sha256",
    )
    for name, sha256 in m.tokenizer.files.items():
        _check(directory / m.tokenizer.dir / name, sha256, f"tokenizer.files[{name!r}]")
