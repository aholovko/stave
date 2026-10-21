"""Request validation before serving.

Paths use the host's root so the same cases work on every platform.
"""

import json
import os
import re
from typing import Any

import pytest

from stave_infer import request

SCHEMA = "stave-advisory-v1"
SHA = "ab" * 32
ROOT = os.path.abspath(os.sep)


def _abs(*parts: str) -> str:
    return os.path.join(ROOT, *parts)


def _payload(document: Any) -> bytes:
    return json.dumps(document).encode("utf-8")


def _request(*files: Any, **extra: Any) -> bytes:
    return _payload({"schema": SCHEMA, "files": list(files), **extra})


def test_a_request_yields_its_files_in_request_order():
    payload = _request(
        {"path": _abs("x", "b.go"), "sha256": "0" * 64},
        {"path": _abs("x", "grüße", "a.go"), "sha256": SHA},
    )

    files = request.parse_request(payload)

    assert [(file.path, file.sha256) for file in files] == [
        (_abs("x", "b.go"), "0" * 64),
        (_abs("x", "grüße", "a.go"), SHA),
    ]


def test_a_request_without_files_is_valid_and_names_none():
    assert request.parse_request(_request()) == []


@pytest.mark.skipif(os.name != "nt", reason="Windows path syntax")
def test_a_windows_drive_path_with_backslashes_is_accepted():
    path = "C:\\Users\\a b\\x_test.go"
    [file] = request.parse_request(_request({"path": path, "sha256": SHA}))
    assert file.path == path


@pytest.mark.parametrize(
    ("payload", "names"),
    [
        pytest.param(b"", "JSON", id="empty-input"),
        pytest.param(b'{"schema":', "JSON", id="truncated-json"),
        pytest.param(_request() + _request(), "JSON", id="two-documents"),
        pytest.param(b'{"schema":"\xff"}', "UTF-8", id="invalid-utf8"),
        pytest.param(_payload([]), "object", id="not-an-object"),
        pytest.param(
            _payload({"schema": "stave-advisory-v2", "files": []}),
            "schema",
            id="other-schema",
        ),
        pytest.param(_payload({"files": []}), "schema", id="missing-schema"),
        pytest.param(_payload({"schema": SCHEMA}), "'files'", id="missing-files"),
        pytest.param(_request(backend="a5"), "'backend'", id="unknown-field"),
        pytest.param(
            _payload(
                {"schema": SCHEMA, "files": {"path": _abs("a.go"), "sha256": SHA}}
            ),
            "list",
            id="files-not-a-list",
        ),
        pytest.param(_request(_abs("a.go")), "files[0]", id="file-not-an-object"),
        pytest.param(_request({"path": _abs("a.go")}), "'sha256'", id="missing-sha256"),
        pytest.param(_request({"sha256": SHA}), "'path'", id="missing-path"),
        pytest.param(
            _request({"path": _abs("a.go"), "sha256": SHA, "status": "checked"}),
            "'status'",
            id="unknown-file-field",
        ),
        pytest.param(
            _request({"path": 7, "sha256": SHA}), "'path'", id="path-not-a-string"
        ),
        pytest.param(
            _request({"path": "", "sha256": SHA}), "absolute", id="empty-path"
        ),
        pytest.param(
            _request({"path": os.path.join("x", "a.go"), "sha256": SHA}),
            "absolute",
            id="relative-path",
        ),
        pytest.param(
            _request({"path": _abs("x", "..", "a.go"), "sha256": SHA}),
            "normalized",
            id="unnormalized-path",
        ),
        pytest.param(
            _request({"path": _abs("a.go"), "sha256": SHA.upper()}),
            "sha256",
            id="uppercase-sha256",
        ),
        pytest.param(
            _request({"path": _abs("a.go"), "sha256": SHA[:-1]}),
            "sha256",
            id="short-sha256",
        ),
        pytest.param(
            _request({"path": _abs("a.go"), "sha256": None}), "sha256", id="null-sha256"
        ),
        pytest.param(
            _request(
                {"path": _abs("x", "b.go"), "sha256": SHA},
                {"path": _abs("x", "a.go"), "sha256": SHA},
                {"path": _abs("x", "b.go"), "sha256": "0" * 64},
            ),
            "files[2]",
            id="repeated-path",
        ),
    ],
)
def test_a_malformed_request_is_refused_naming_the_violation(payload, names):
    with pytest.raises(request.RequestError, match=re.escape(names)) as refused:
        request.parse_request(payload)

    assert isinstance(refused.value, ValueError)
    assert str(refused.value).startswith(f"{SCHEMA}: ")
