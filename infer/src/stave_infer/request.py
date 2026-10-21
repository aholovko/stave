"""Validate the files and expected bytes in a `stave-advisory-v1` request.

Paths must be unique, normalized absolute paths on the host so responses can match them by exact equality.
Malformed requests are rejected before any selector or model runs.
"""

import dataclasses
import json
import os
from typing import Any, NoReturn

SCHEMA = "stave-advisory-v1"

_REQUEST_FIELDS = frozenset({"schema", "files"})
_FILE_FIELDS = frozenset({"path", "sha256"})


class RequestError(ValueError):
    """A malformed request. Nothing is served."""


@dataclasses.dataclass(frozen=True)
class RequestFile:
    """One requested source revision: where to read it and the bytes expected there."""

    path: str
    sha256: str


def _err(msg: str) -> NoReturn:
    raise RequestError(f"{SCHEMA}: {msg}")


def _fields(value: Any, context: str, allowed: frozenset[str]) -> dict[str, Any]:
    """Return `value` as an object that carries exactly the allowed fields."""
    if not isinstance(value, dict):
        _err(f"{context} must be a JSON object")
    unknown = sorted(set(value) - allowed)
    if unknown:
        _err(f"{context} has unknown field {unknown[0]!r}")
    missing = sorted(allowed - set(value))
    if missing:
        _err(f"{context} is missing field {missing[0]!r}")
    return value


def _is_lowhex(s: Any, n: int) -> bool:
    return (
        isinstance(s, str) and len(s) == n and all(c in "0123456789abcdef" for c in s)
    )


def parse_request(payload: bytes) -> list[RequestFile]:
    """Return files in request order, rejecting malformed input before serving any file."""
    try:
        document = json.loads(payload.decode("utf-8"))
    except UnicodeDecodeError as exc:
        _err(f"request is not valid UTF-8: {exc}")
    except json.JSONDecodeError as exc:
        _err(f"request is not valid JSON: {exc.msg}")
    if not isinstance(document, dict):
        _err("request must be a JSON object")
    if document.get("schema") != SCHEMA:
        _err(f"request schema must be {SCHEMA!r}, got {document.get('schema')!r}")
    request = _fields(document, "request", _REQUEST_FIELDS)
    if not isinstance(request["files"], list):
        _err("request files must be a list")
    files: list[RequestFile] = []
    seen: set[str] = set()
    for index, value in enumerate(request["files"]):
        context = f"files[{index}]"
        entry = _fields(value, context, _FILE_FIELDS)
        path = entry["path"]
        if not isinstance(path, str):
            _err(f"{context} field 'path' must be a string")
        if not os.path.isabs(path) or os.path.normpath(path) != path:
            _err(f"{context} path must be a normalized absolute path, got {path!r}")
        if not _is_lowhex(entry["sha256"], 64):
            _err(f"{context} sha256 must be 64 lowercase hex chars")
        if path in seen:
            _err(f"{context} repeats path {path!r}")
        seen.add(path)
        files.append(RequestFile(path, entry["sha256"]))
    return files
