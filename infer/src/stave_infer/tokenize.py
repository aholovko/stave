"""Assemble source runs and encode them with the UniXcoder tokenizer.

Runs are tokenized separately to preserve the checkpoint's framing and separator rules. Inverting the byte-level
mapping must reproduce every source byte, so token spans can be traced to exact source positions. The slow
RobertaTokenizer and its `<mask0>` registration are part of this input contract. Loading stays offline and checks
the five pinned files before use.
"""

import dataclasses
import hashlib
import json
import os
from collections.abc import Mapping, Sequence
from pathlib import Path
from typing import Any

from stave_infer.bundle import TOKENIZER_FILES

MODE = "<encoder-only>"
FRAMING_TOKENS = 4
#: UniXcoder requires this extra special-token registration.
MASK0 = "<mask0>"
#: Bound memory use before hashing; the tokenizer vocabulary is well below this limit.
MAX_TOKENIZER_BYTES = 8 * 1024 * 1024


class TokenizeError(ValueError):
    """An assembly, encoding or tokenizer identity failure."""


@dataclasses.dataclass(frozen=True)
class Piece:
    """Retained source bytes starting at a file-absolute byte offset."""

    path: str
    sha256: str
    start: int
    data: bytes


@dataclasses.dataclass(frozen=True)
class Run:
    """One maximal contiguous admitted run of one source file."""

    path: str
    sha256: str
    start: int
    data: bytes


@dataclasses.dataclass(frozen=True)
class Token:
    token: str
    id: int
    file: tuple[str, str] | None
    start: int | None
    end: int | None
    literal_special: bool


@dataclasses.dataclass(frozen=True)
class Encoding:
    tokens: tuple[Token, ...]
    payload_tokens: int
    separator_tokens: int
    codepoint_split_tokens: int
    literal_specials: list[str]
    framing_tokens: int = FRAMING_TOKENS

    @property
    def total_tokens(self) -> int:
        return len(self.tokens)

    def fits(self, budget: int) -> bool:
        return self.total_tokens <= budget


# Assembly and encoding


def assemble(
    pieces: Sequence[Piece], file_order: Sequence[tuple[str, str]], record_id: str
) -> list[Run]:
    """Return the byte-interval union of `pieces` as maximal runs in file order."""
    order = {key: index for index, key in enumerate(file_order)}
    for piece in pieces:
        if (piece.path, piece.sha256) not in order:
            raise TokenizeError(
                f"{record_id}: piece of {piece.path} {piece.sha256} is not in the record's file order"
            )
        if not piece.data:
            raise TokenizeError(
                f"{record_id}: empty piece at {piece.path}:{piece.start}"
            )
    merged: list[tuple[str, str, int, bytearray]] = []
    for piece in sorted(pieces, key=lambda p: (order[(p.path, p.sha256)], p.start)):
        end = piece.start + len(piece.data)
        if merged and merged[-1][:2] == (piece.path, piece.sha256):
            path, sha, start, data = merged[-1]
            current_end = start + len(data)
            if piece.start <= current_end:
                overlap_end = min(current_end, end)
                if (
                    data[piece.start - start : overlap_end - start]
                    != piece.data[: overlap_end - piece.start]
                ):
                    raise TokenizeError(
                        f"{record_id}: conflicting bytes where pieces overlap in {path} "
                        f"[{piece.start}, {overlap_end})"
                    )
                if end > current_end:
                    data.extend(piece.data[current_end - piece.start :])
                continue
        merged.append((piece.path, piece.sha256, piece.start, bytearray(piece.data)))
    return [Run(path, sha, start, bytes(data)) for path, sha, start, data in merged]


def _synthetic(token: str) -> Token:
    return Token(token, -1, None, None, None, False)


def encode(tokenizer: Any, runs: Sequence[Run], record_id: str) -> Encoding:
    """Frame source runs for encoder-only input, retaining absolute byte spans."""
    added: Mapping[str, int] = tokenizer.added_tokens_encoder
    decoder: Mapping[str, int] = tokenizer.byte_decoder
    tokens = [
        _synthetic(tokenizer.cls_token),
        _synthetic(MODE),
        _synthetic(tokenizer.sep_token),
    ]
    payload = separators = splits = 0
    specials: list[str] = []
    for number, run in enumerate(runs):
        where = f"{record_id}: run {number} ({run.path} at {run.start})"
        if number:
            tokens.append(_synthetic(tokenizer.sep_token))
            separators += 1
        try:
            text = run.data.decode("utf-8", errors="strict")
        except UnicodeDecodeError as exc:
            raise TokenizeError(f"{where} is not strict UTF-8: {exc}") from exc
        pieces: list[str] = tokenizer.tokenize(text)
        run_specials = [p for p in pieces if p in added]
        boundaries = {i for i, b in enumerate(run.data) if b & 0xC0 != 0x80}
        boundaries.add(len(run.data))
        offset = 0
        for piece in pieces:
            special = piece in added
            try:
                raw = (
                    piece.encode("utf-8")
                    if special
                    else bytes(decoder[c] for c in piece)
                )
            except KeyError as exc:
                raise TokenizeError(
                    f"{where}: token {piece!r} has no inverse byte mapping"
                ) from exc
            source = run.data[offset : offset + len(raw)]
            if not raw or source != raw:
                raise TokenizeError(
                    f"{where}: token {piece!r} bytes {raw!r} do not reproduce source bytes "
                    f"{source!r} at {run.path}:{run.start + offset}; literal special tokens "
                    f"in run: {run_specials}"
                )
            if offset not in boundaries or offset + len(raw) not in boundaries:
                splits += 1
            tokens.append(
                Token(
                    piece,
                    -1,
                    (run.path, run.sha256),
                    run.start + offset,
                    run.start + offset + len(raw),
                    special,
                )
            )
            offset += len(raw)
        if offset != len(run.data):
            raise TokenizeError(
                f"{where}: tokens reproduce {offset} of {len(run.data)} bytes; literal special "
                f"tokens in run: {run_specials}"
            )
        payload += len(pieces)
        specials.extend(run_specials)
    tokens.append(_synthetic(tokenizer.sep_token))
    ids: list[int] = tokenizer.convert_tokens_to_ids([t.token for t in tokens])
    framed = tuple(
        dataclasses.replace(t, id=i) for t, i in zip(tokens, ids, strict=True)
    )
    return Encoding(framed, payload, separators, splits, specials)


# Tokenizer


def _read_limited(path: Path, limit: int) -> bytes:
    try:
        size = path.stat().st_size
        if size > limit:
            raise TokenizeError(f"{path}: {size} bytes exceed the {limit}-byte limit")
        return path.read_bytes()
    except OSError as exc:
        raise TokenizeError(f"{path}: unreadable: {exc}") from exc


# Opening a bundle in Finder or Explorer must not make its tokenizer invalid.
_BROWSER_FILES = frozenset({".DS_Store", "Thumbs.db"})


def verify_tokenizer_files(tokenizer_dir: Path, files: Mapping[str, str]) -> None:
    """Reject unpinned tokenizer inputs and verify all five file hashes.

    Transformers also reads sidecars such as `added_tokens.json`, which can change token ids without changing
    the pinned files. Only harmless file-browser metadata is allowed alongside them.
    """
    missing = [name for name in TOKENIZER_FILES if name not in files]
    if missing:
        raise TokenizeError(f"tokenizer files {missing} are not pinned")
    try:
        extra = sorted(
            p.name
            for p in tokenizer_dir.iterdir()
            if p.name not in TOKENIZER_FILES and p.name not in _BROWSER_FILES
        )
    except OSError as exc:
        raise TokenizeError(f"{tokenizer_dir}: unreadable: {exc}") from exc
    if extra:
        raise TokenizeError(
            f"tokenizer directory holds unpinned entries: {', '.join(extra)}"
        )
    for name in TOKENIZER_FILES:
        path = tokenizer_dir / name
        digest = hashlib.sha256(_read_limited(path, MAX_TOKENIZER_BYTES)).hexdigest()
        if digest != files[name]:
            raise TokenizeError(
                f"{path}: sha256 {digest} is not the pinned {files[name]}"
            )


def load_tokenizer(tokenizer_dir: Path, files: Mapping[str, str]) -> Any:
    """Load the verified slow RobertaTokenizer with the required `<mask0>` registration."""
    verify_tokenizer_files(tokenizer_dir, files)
    os.environ["HF_HUB_OFFLINE"] = "1"
    os.environ["TRANSFORMERS_OFFLINE"] = "1"
    from transformers import RobertaTokenizer

    tokenizer: Any = RobertaTokenizer.from_pretrained(
        str(tokenizer_dir), local_files_only=True
    )
    if tokenizer.is_fast or type(tokenizer).__name__ != "RobertaTokenizer":
        raise TokenizeError(
            f"{type(tokenizer).__name__} is not the slow RobertaTokenizer"
        )
    tokenizer.add_tokens([MASK0], special_tokens=True)
    config = json.loads((tokenizer_dir / "config.json").read_text(encoding="utf-8"))
    mask0 = tokenizer.convert_tokens_to_ids(MASK0)
    if not 0 <= mask0 < config["vocab_size"]:
        raise TokenizeError(
            f"{MASK0} id {mask0} is outside the pinned config vocabulary"
        )
    framing = tokenizer.convert_tokens_to_ids(
        [tokenizer.cls_token, MODE, tokenizer.sep_token]
    )
    if len(set(framing)) != 3 or tokenizer.unk_token_id in framing:
        raise TokenizeError(f"framing ids {framing} are not distinct known tokens")
    return tokenizer
