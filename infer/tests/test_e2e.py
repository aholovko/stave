"""Installed bundles through the CLI and headless LSP.

A temporary STAVE_HOME and the test interpreter isolate installation from the user's runtime. Fixture paths
include spaces and non-ASCII characters to exercise the Go/Python boundary, and avoid the TF-excluded `testdata`.

Backend reference outputs are mapped to report diagnostics and coverage: reports omit scores for negative
predictions. The LSP checks require completion with the selected bundle's identity.
"""

import json
import os
import queue
import shutil
import subprocess
import sys
import threading
import time
from collections.abc import Callable
from pathlib import Path
from typing import IO, Any

from support import FIXTURES, TOKENIZER_DIR, copy_fixture_package, go_stave

from stave_infer import toy

TOLERANCE = 1e-4
NO_SPAN = (-1, -1)
LSP_TIMEOUT = 180.0

Message = dict[str, Any]


def _stave(env: dict[str, str], *args: str) -> subprocess.CompletedProcess[str]:
    return subprocess.run(
        [str(go_stave()), *args],
        env=env,
        capture_output=True,
        text=True,
        encoding="utf-8",
        check=False,
    )


def _ok(result: subprocess.CompletedProcess[str]) -> str:
    assert result.returncode == 0, (result.args, result.stdout, result.stderr)
    return result.stdout


def _report(env: dict[str, str], pkg: Path) -> dict[str, Any]:
    out = _ok(_stave(env, "check", "-json", str(pkg)))
    lines = out.splitlines()
    assert len(lines) == 1, out
    return json.loads(lines[0])


def _span(span: Any) -> tuple[int, int]:
    if span is None:
        return NO_SPAN
    if isinstance(span, dict):
        return (span["start"], span["end"])
    return (span[0], span[1])


def _compare(report: dict[str, Any], expected: dict[str, Any], pkg: Path) -> None:
    advisory = report["advisory"]
    assert advisory["status"] == "ok", advisory
    assert advisory["backend"]["id"] == expected["backend"]["id"]
    assert advisory["backend"]["detail"] == expected["backend"]["detail"]
    assert advisory["classes"] == expected["classes"]

    files = {f["name"]: f for f in report["files"]}
    assert sorted(files) == sorted(r["file"] for r in expected["files"])
    for name, f in files.items():
        assert f["path"] == str(pkg / name), "a request path did not round-trip"
        assert f["role"] in ("target", "test-target")

    advised = sorted(
        (p["file"], p["class"], _span(p["span"]), p["score"])
        for p in expected["predictions"]
        if p["coverage"] == "checked" and p["advice"]
    )
    reported = sorted(
        (name, d["class"], _span(d["span"]), d["score"])
        for name, f in files.items()
        for d in f["diagnostics"]
        if d["origin"] == "advisory"
    )
    assert [a[:3] for a in reported] == [a[:3] for a in advised]
    for got, want in zip(reported, advised, strict=True):
        assert abs(got[3] - want[3]) <= TOLERANCE, (got, want)
    for name, f in files.items():
        for d in f["diagnostics"]:
            if d["origin"] == "advisory":
                assert d["path"] == str(pkg / name)
                assert d["backend"]["id"] == expected["backend"]["id"]

    for record in expected["files"]:
        f = files[record["file"]]
        if "context" in record:
            assert report["context"]["digest"] == record["context"]
        for c in f["coverage"]:
            if c["origin"] != "advisory":
                continue
            assert record["status"] == "checked"
            if c["class"] not in record["classes"]:
                assert c["status"] == "unsupported", c
                assert "did not check this class" in c["reason"], c
                assert "candidates" not in c, c
                continue
            predictions = [
                p
                for p in expected["predictions"]
                if p["file"] == record["file"] and p["class"] == c["class"]
            ]
            withheld = record.get("withheld", {}).get(c["class"], {})
            items = sorted(
                [
                    (_span(p["span"]), p["coverage"], p["reason"])
                    for p in predictions
                    if p["coverage"] != "checked"
                ]
                + [
                    (NO_SPAN, "unsupported", reason)
                    for reason, count in withheld.items()
                    for _ in range(count)
                ]
            )
            got = sorted(
                (_span(i.get("span")), i["status"], i["reason"]) for i in c["items"]
            )
            assert got == items, (record["file"], c["class"])
            assert c["candidates"] == len(predictions) + sum(withheld.values())
            want_status = "checked"
            if items:
                statuses = {status for _, status, _ in items}
                want_status = "error" if "error" in statuses else "unsupported"
            assert c["status"] == want_status, (record["file"], c)


def test_installed_path_end_to_end(tmp_path: Path) -> None:
    env = {
        **os.environ,
        "STAVE_HOME": str(tmp_path / "home"),
        "STAVE_RUNTIME_PYTHON": sys.executable,
    }
    toy.make_toy_bundle("S", tmp_path / "toy-S", tokenizer_dir=TOKENIZER_DIR)
    archive = shutil.make_archive(
        str(tmp_path / "toy-S"), "zip", root_dir=tmp_path, base_dir="toy-S"
    )
    toy.make_toy_bundle("B", tmp_path / "toy-B", tokenizer_dir=TOKENIZER_DIR)
    pkg = copy_fixture_package(tmp_path / "stave é 😀 test")

    out = _ok(_stave(env, "model", "add", archive))
    assert "active model: toy-S" in out
    assert (tmp_path / "home" / "models" / "toy-S" / "manifest.json").is_file()

    report = _report(env, pkg)
    expected = json.loads((FIXTURES / "toy-expected" / "S.json").read_text())
    _compare(report, expected, pkg)

    _ok(_stave(env, "model", "add", str(tmp_path / "toy-B")))
    _ok(_stave(env, "model", "use", "toy-B"))
    switched = _report(env, pkg)
    assert switched["advisory"]["status"] == "ok", switched["advisory"]
    assert switched["advisory"]["backend"]["id"] == "toy-B"
    assert switched["advisory"]["backend"]["id"] != report["advisory"]["backend"]["id"]


def _frame(message: Message) -> bytes:
    body = json.dumps({"jsonrpc": "2.0", **message}).encode()
    return b"Content-Length: %d\r\n\r\n" % len(body) + body


def _read_frames(stream: IO[bytes], into: queue.Queue[Message | None]) -> None:
    """Put each framed message read from `stream` on the queue, then None when the stream ends."""
    while True:
        length = -1
        while line := stream.readline().rstrip(b"\r\n"):
            name, _, value = line.partition(b":")
            if name.strip().lower() == b"content-length":
                length = int(value)
        if length < 0:
            into.put(None)
            return
        into.put(json.loads(stream.read(length)))


def _await(
    messages: queue.Queue[Message | None],
    what: str,
    accept: Callable[[Message], bool],
    deadline: float,
) -> Message:
    """Return the first message accepted before the deadline. The others are dropped."""
    while True:
        try:
            message = messages.get(timeout=max(deadline - time.monotonic(), 0))
        except queue.Empty:
            raise AssertionError(f"no {what} within {LSP_TIMEOUT} s") from None
        assert message is not None, f"the server ended before {what}"
        if accept(message):
            return message


def test_headless_lsp_session_names_each_toy_bundle(tmp_path: Path) -> None:
    env = {
        **os.environ,
        "STAVE_HOME": str(tmp_path / "home"),
        "STAVE_RUNTIME_PYTHON": sys.executable,
    }
    for variant in ("S", "B"):
        bundle = tmp_path / f"toy-{variant}"
        toy.make_toy_bundle(variant, bundle, tokenizer_dir=TOKENIZER_DIR)
        _ok(_stave(env, "model", "add", str(bundle)))
    demo = copy_fixture_package(tmp_path / "lsp") / "demo.go"
    uri = demo.as_uri()

    server = subprocess.Popen(
        [str(go_stave()), "lsp", "--stdio"],
        env=env,
        stdin=subprocess.PIPE,
        stdout=subprocess.PIPE,
        stderr=subprocess.DEVNULL,
    )
    assert server.stdin is not None and server.stdout is not None
    stdin, stdout = server.stdin, server.stdout
    messages: queue.Queue[Message | None] = queue.Queue()
    threading.Thread(target=_read_frames, args=(stdout, messages), daemon=True).start()

    def send(message: Message) -> None:
        stdin.write(_frame(message))
        stdin.flush()

    def answer(id: int) -> Callable[[Message], bool]:
        return lambda m: m.get("id") == id and "method" not in m

    def complete(m: Message) -> bool:
        if m.get("method") != "stave/status" or m["params"]["uri"] != uri:
            return False
        assert m["params"]["state"] != "failed", m["params"]
        return m["params"]["state"] == "complete"

    def names(status: Message, backend: str) -> bool:
        detail = status["params"]["detail"]
        return any(line.startswith(f"backend {backend} (") for line in detail)

    try:
        deadline = time.monotonic() + LSP_TIMEOUT
        send(
            {
                "id": 1,
                "method": "initialize",
                "params": {"processId": None, "rootUri": None, "capabilities": {}},
            }
        )
        _await(messages, "initialize response", answer(1), deadline)
        send({"method": "initialized", "params": {}})
        send(
            {
                "method": "textDocument/didOpen",
                "params": {
                    "textDocument": {
                        "uri": uri,
                        "languageId": "go",
                        "version": 1,
                        "text": demo.read_bytes().decode("utf-8"),
                    }
                },
            }
        )
        save = {
            "method": "textDocument/didSave",
            "params": {"textDocument": {"uri": uri}},
        }
        send(save)
        status = _await(messages, "complete status", complete, deadline)
        assert names(status, "toy-S"), status["params"]

        _ok(_stave(env, "model", "use", "toy-B"))
        send(save)
        deadline = time.monotonic() + LSP_TIMEOUT
        status = _await(
            messages, "complete status after the switch", complete, deadline
        )
        assert names(status, "toy-B"), status["params"]

        send({"id": 2, "method": "shutdown", "params": None})
        _await(messages, "shutdown response", answer(2), deadline)
        send({"method": "exit", "params": None})
        assert server.wait(timeout=20) == 0
    finally:
        if server.poll() is None:
            server.kill()
            server.wait()
        stdin.close()
        stdout.close()


def test_an_incompatible_bundle_is_refused_with_the_runtimes_reason(
    tmp_path: Path,
) -> None:
    env = {
        **os.environ,
        "STAVE_HOME": str(tmp_path / "home"),
        "STAVE_RUNTIME_PYTHON": sys.executable,
    }
    bundle = tmp_path / "toy-S"
    toy.make_toy_bundle("S", bundle, tokenizer_dir=TOKENIZER_DIR)
    manifest = json.loads((bundle / "manifest.json").read_text())
    manifest["seed"] = manifest["seed"] + 1
    (bundle / "manifest.json").write_text(json.dumps(manifest))

    result = _stave(env, "model", "add", str(bundle))
    assert result.returncode == 1
    assert "stave_infer:" in result.stderr and "seed" in result.stderr
    assert not (tmp_path / "home" / "models" / "toy-S").exists()
