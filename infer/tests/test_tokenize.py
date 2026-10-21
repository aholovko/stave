"""Byte assembly and tokenizer compatibility.

The byte tokenizer isolates span behavior; the pinned UniXcoder fixtures check the actual input contract.
"""

import re
from pathlib import Path
from typing import Any

import pytest
from support import TOKENIZER_DIR, copy_tokenizer, tokenizer_files

from stave_infer import tokenize as tk

# Toy byte-level tokenizer
# One token per byte makes span checks exact; special-token handling exercises whitespace loss.


class ByteTokenizer:
    cls_token = "<s>"
    sep_token = "</s>"
    unk_token = "<unk>"

    def __init__(self) -> None:
        self.byte_decoder = {chr(0x100 + b): b for b in range(256)}
        self.added_tokens_encoder = {
            "<s>": 0,
            "<pad>": 1,
            "</s>": 2,
            "<unk>": 3,
            "<mask>": 4,
        }
        self._specials = re.compile(
            "(" + "|".join(re.escape(t) for t in self.added_tokens_encoder) + ")"
        )

    def tokenize(self, text: str) -> list[str]:
        chunks = self._specials.split(text)
        tokens: list[str] = []
        for index, chunk in enumerate(chunks):
            if index % 2:
                tokens.append(chunk)
                continue
            if index + 1 < len(chunks) and chunks[index + 1] == "<mask>":
                chunk = chunk.rstrip()
            tokens.extend(chr(0x100 + b) for b in chunk.encode("utf-8"))
        return tokens

    def convert_tokens_to_ids(self, tokens: list[str]) -> list[int]:
        ids: list[int] = []
        for token in tokens:
            if token in self.added_tokens_encoder:
                ids.append(self.added_tokens_encoder[token])
            elif token == tk.MODE:
                ids.append(300)
            elif token in self.byte_decoder:
                ids.append(10 + self.byte_decoder[token])
            else:
                ids.append(3)
        return ids


TOY = ByteTokenizer()
SHA = "a" * 64


def _run(data: bytes, start: int = 0, path: str = "a.go", sha: str = SHA) -> tk.Run:
    return tk.Run(path=path, sha256=sha, start=start, data=data)


# Encoding


def test_ascii_payload_tiles_run_and_total_adds_framing() -> None:
    encoding = tk.encode(TOY, [_run(b"x := 1\n", start=40)], "r1")

    assert encoding.payload_tokens == 7
    assert encoding.framing_tokens == 4
    assert encoding.separator_tokens == 0
    assert encoding.total_tokens == 11
    payload = encoding.tokens[3:-1]
    assert [(t.start, t.end) for t in payload] == [(40 + i, 41 + i) for i in range(7)]
    assert all(t.file == ("a.go", SHA) for t in payload)


def test_framing_tokens_follow_encoder_only_pattern_with_null_extents() -> None:
    encoding = tk.encode(TOY, [_run(b"x")], "r1")

    framing = [encoding.tokens[i] for i in (0, 1, 2, -1)]
    assert [t.token for t in framing] == ["<s>", tk.MODE, "</s>", "</s>"]
    assert [t.id for t in framing] == [0, 300, 2, 2]
    assert all(t.file is None and t.start is None and t.end is None for t in framing)


def test_multibyte_code_point_split_is_counted() -> None:
    encoding = tk.encode(TOY, [_run("aé".encode())], "r1")

    assert encoding.payload_tokens == 3
    assert encoding.codepoint_split_tokens == 2


def test_crlf_bytes_are_reconstructed_exactly() -> None:
    data = b"a\r\nb\r\n"
    encoding = tk.encode(TOY, [_run(data)], "r1")

    rebuilt = b"".join(
        data[t.start : t.end]
        for t in encoding.tokens
        if t.start is not None and t.end is not None
    )
    assert rebuilt == data
    assert encoding.payload_tokens == 6


def test_literal_special_token_in_source_is_flagged_and_source_mapped() -> None:
    encoding = tk.encode(TOY, [_run(b's := "</s>"', start=100)], "r1")

    special = [t for t in encoding.tokens if t.literal_special]
    assert [(t.token, t.start, t.end) for t in special] == [("</s>", 106, 110)]
    assert encoding.literal_specials == ["</s>"]


def test_mask_absorbing_preceding_space_fails_identity_with_record_detail() -> None:
    with pytest.raises(tk.TokenizeError, match=r"rec-7.*<mask>.*literal special"):
        tk.encode(TOY, [_run(b"a <mask>")], "rec-7")


def test_invalid_utf8_run_is_an_identity_error() -> None:
    with pytest.raises(tk.TokenizeError, match=r"r1.*UTF-8"):
        tk.encode(TOY, [_run(b"a\xffb")], "r1")


def test_disjoint_runs_get_one_synthetic_separator() -> None:
    encoding = tk.encode(TOY, [_run(b"ab", start=0), _run(b"cd", start=10)], "r1")

    assert encoding.separator_tokens == 1
    assert encoding.total_tokens == 4 + 4 + 1
    separator = encoding.tokens[5]
    assert (
        separator.token == "</s>"
        and separator.file is None
        and not separator.literal_special
    )
    assert [(t.start, t.end) for t in encoding.tokens[6:8]] == [(10, 11), (11, 12)]


@pytest.mark.parametrize("budget", (512, 1023))
def test_budget_fit_is_inclusive_and_one_over_does_not_fit(budget: int) -> None:
    exact = tk.encode(TOY, [_run(b"x" * (budget - 4))], "r1")
    over = tk.encode(TOY, [_run(b"x" * (budget - 3))], "r1")

    assert exact.total_tokens == budget and exact.fits(budget)
    assert over.total_tokens == budget + 1 and not over.fits(budget)


# Assembly


def test_overlapping_pieces_are_admitted_once() -> None:
    source = b"// doc\nfunc f() {}\n"
    target = tk.Piece("a.go", SHA, 100, source[:12])
    declaration = tk.Piece("a.go", SHA, 107, source[7:])

    runs = tk.assemble([target, declaration], [("a.go", SHA)], "r1")

    assert runs == [_run(source, start=100)]


def test_adjacent_pieces_merge_and_disjoint_pieces_stay_apart() -> None:
    pieces = [
        tk.Piece("a.go", SHA, 20, b"cd"),
        tk.Piece("a.go", SHA, 0, b"ab"),
        tk.Piece("a.go", SHA, 2, b"xy"),
    ]

    runs = tk.assemble(pieces, [("a.go", SHA)], "r1")

    assert runs == [_run(b"abxy", start=0), _run(b"cd", start=20)]


def test_runs_follow_the_given_file_order() -> None:
    other = "b" * 64
    pieces = [tk.Piece("z.go", other, 0, b"z"), tk.Piece("a.go", SHA, 5, b"a")]

    runs = tk.assemble(pieces, [("z.go", other), ("a.go", SHA)], "r1")

    assert [(r.path, r.start) for r in runs] == [("z.go", 0), ("a.go", 5)]


def test_conflicting_overlap_bytes_fail() -> None:
    pieces = [tk.Piece("a.go", SHA, 0, b"abc"), tk.Piece("a.go", SHA, 1, b"Xc")]

    with pytest.raises(tk.TokenizeError, match=r"r1.*conflict"):
        tk.assemble(pieces, [("a.go", SHA)], "r1")


def test_piece_outside_file_order_fails() -> None:
    with pytest.raises(tk.TokenizeError, match=r"r1.*file order"):
        tk.assemble([tk.Piece("b.go", SHA, 0, b"x")], [("a.go", SHA)], "r1")


# Verified UniXcoder tokenizer


@pytest.fixture(scope="module")
def unixcoder(tmp_path_factory: pytest.TempPathFactory) -> Any:
    directory = copy_tokenizer(tmp_path_factory.mktemp("tokenizer"))
    return tk.load_tokenizer(directory, tokenizer_files())


def _official_ids(tokenizer: Any, text: str, max_length: int = 1023) -> list[int]:
    # Match UniXcoder's encoder-only framing independently of the production encoder.
    tokens = tokenizer.tokenize(text)
    tokens = tokens[: max_length - 4]
    tokens = [
        tokenizer.cls_token,
        "<encoder-only>",
        tokenizer.sep_token,
        *tokens,
        tokenizer.sep_token,
    ]
    return tokenizer.convert_tokens_to_ids(tokens)


GO_FIXTURE = (
    "// Parse reads a café config.\r\n"
    "func Parse(r io.Reader) (*Config, error) {\n"
    # This emoji splits across tokens, exercising boundaries inside a code point.
    '\tif r == nil {\n\t\treturn nil, errors.New("nil reader — 日本 🦫")\n\t} else {\n'
    "\t\treturn decode(r)\n\t}\n}\n"
)


def test_real_tokenizer_is_the_verified_slow_class(unixcoder: Any) -> None:
    assert type(unixcoder).__name__ == "RobertaTokenizer" and unixcoder.is_fast is False
    assert unixcoder.add_prefix_space is False
    assert len(unixcoder) == 51416
    mask0 = unixcoder.convert_tokens_to_ids(tk.MASK0)
    assert 0 <= mask0 < 51416 and tk.MASK0 in unixcoder.added_tokens_encoder
    ids = unixcoder.convert_tokens_to_ids(
        [unixcoder.cls_token, tk.MODE, unixcoder.sep_token]
    )
    assert len(set(ids)) == 3 and unixcoder.unk_token_id not in ids


def test_real_payload_ids_match_official_wrapper_and_reconstruct(
    unixcoder: Any,
) -> None:
    data = GO_FIXTURE.encode()
    encoding = tk.encode(unixcoder, [_run(data, start=7)], "fixture")

    assert [t.id for t in encoding.tokens] == _official_ids(unixcoder, GO_FIXTURE)
    rebuilt = b"".join(
        data[t.start - 7 : t.end - 7]
        for t in encoding.tokens
        if t.start is not None and t.end is not None
    )
    assert rebuilt == data
    assert encoding.codepoint_split_tokens > 0


@pytest.mark.parametrize("literal", ["</s>", "<mask0>", "<s>"])
def test_real_literal_special_token_is_mapped_flagged_and_matches_wrapper(
    unixcoder: Any, literal: str
) -> None:
    text = f'x := "{literal}"\n'
    encoding = tk.encode(unixcoder, [_run(text.encode())], "lit")

    assert [t.id for t in encoding.tokens] == _official_ids(unixcoder, text)
    special = [t for t in encoding.tokens if t.literal_special]
    assert [(t.token, t.start, t.end) for t in special] == [
        (literal, 6, 6 + len(literal))
    ]


def test_real_mask_with_leading_space_is_an_identity_failure(unixcoder: Any) -> None:
    with pytest.raises(tk.TokenizeError, match=r"mask-rec.*<mask>.*literal special"):
        tk.encode(unixcoder, [_run(b"x := a <mask>\n")], "mask-rec")


def _copy(tmp_path: Path) -> Path:
    return copy_tokenizer(tmp_path / "tokenizer")


def test_a_changed_merges_file_is_refused_before_loading(tmp_path: Path) -> None:
    directory = _copy(tmp_path)
    files = tokenizer_files()
    with (directory / "merges.txt").open("ab") as f:
        f.write(b"a b\n")
    with pytest.raises(tk.TokenizeError, match=r"merges\.txt.*sha256"):
        tk.load_tokenizer(directory, files)


def test_every_tokenizer_file_must_be_pinned(tmp_path: Path) -> None:
    files = tokenizer_files()
    del files["config.json"]
    with pytest.raises(tk.TokenizeError, match=r"config\.json"):
        tk.verify_tokenizer_files(TOKENIZER_DIR, files)
    directory = _copy(tmp_path)
    (directory / "vocab.json").unlink()
    with pytest.raises(tk.TokenizeError, match=r"vocab\.json"):
        tk.verify_tokenizer_files(directory, tokenizer_files())


def test_an_unpinned_tokenizer_entry_is_refused_before_loading(tmp_path: Path) -> None:
    # A sidecar such as added_tokens.json would change the ids that the pinned files define.
    directory = _copy(tmp_path)
    (directory / "added_tokens.json").write_text('{"foobar": 10}')
    with pytest.raises(
        tk.TokenizeError,
        match=r"^tokenizer directory holds unpinned entries: added_tokens\.json$",
    ):
        tk.verify_tokenizer_files(directory, tokenizer_files())
    (directory / "added_tokens.json").unlink()
    (directory / "extra").mkdir()
    with pytest.raises(tk.TokenizeError, match=r"unpinned entries: extra$"):
        tk.load_tokenizer(directory, tokenizer_files())
    (directory / "extra").rmdir()
    for name in (".DS_Store", "Thumbs.db"):  # Harmless file-browser metadata.
        (directory / name).write_bytes(b"metadata")
    tk.verify_tokenizer_files(directory, tokenizer_files())
