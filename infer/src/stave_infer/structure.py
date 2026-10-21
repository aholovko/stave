"""Project selector results into token facts, role memberships and F/S masks.

Facts and role memberships use each token's last source byte, even when a token crosses a role boundary.
Membership uses interval unions so gaps never become part of a role. F pools all source tokens; S pools only
members of each role. Neither pools separators, framing or padding, and identities never enter the facts.
The categorical inventories and caps must retain the widths expected by trained models.
"""

import bisect
import dataclasses
import hashlib
import json
import math
import subprocess
from collections.abc import Mapping, Sequence
from typing import Any

from stave_infer import tokenize as tk

STATEMENTS = (
    "Decl",
    "Empty",
    "Labeled",
    "Expr",
    "Send",
    "IncDec",
    "Assign",
    "Go",
    "Defer",
    "Return",
    "Branch",
    "Block",
    "If",
    "CaseClause",
    "Switch",
    "TypeSwitch",
    "CommClause",
    "Select",
    "For",
    "Range",
)
KINDS = (*STATEMENTS, "NoStmt", "Sep")
PARENTS = (*STATEMENTS, "FuncRoot", "Outside", "Sep", "None")
SLOTS = ("None", "Then", "Else", "LoopBody", "CaseBody", "FuncBody")
ORDINALS = (*(str(i) for i in range(8)), "8+", "None")
LASTS = ("Last", "NotLast", "None")
DEPTHS = (*(str(i) for i in range(7)), "7+", "None")
LITS = ("Root", "Lit1", "Lit2+", "None")
SEGMENTS = ("Source", "Sep", "Frame", "Pad")
INVENTORIES = (KINDS, PARENTS, SLOTS, ORDINALS, LASTS, DEPTHS, LITS, SEGMENTS)
BUCKETS = tuple(len(inventory) for inventory in INVENTORIES)

#: CAND must stay first: model heads take their query from its membership column.
ROLES: dict[str, tuple[str, ...]] = {
    "P1": ("CAND", "IF_HDR", "THEN_BODY", "THEN_LAST", "ELSE_BODY", "FUNC_SIG"),
    "P2": ("CAND", "CALL"),
    "P3": ("CAND", "DECL"),
    "TF": (
        "CAND",
        "ARGS",
        "PREV",
        "ENCL_HDR",
        "RUN_PREFIX",
        "BIND_SIG",
        "FUNC_SIG",
        "BODY",
    ),
    "DC": ("CAND", "DECL"),
    "RP": ("CAND", "DECL", "DOC"),
}
REQUIRED = {
    "P1": ("CAND", "IF_HDR", "THEN_BODY", "ELSE_BODY"),
    "P2": ("CAND", "CALL"),
    "P3": ("CAND", "DECL"),
    "TF": ("CAND", "ARGS", "BIND_SIG", "FUNC_SIG", "BODY"),
    "DC": ("CAND", "DECL"),
    "RP": ("CAND", "DECL"),
}
LEADING_FRAME = 3

Rows = list[list[int]]
Mask = list[list[float]]


class StructureError(ValueError):
    """A source identity or selector result contract failure."""


@dataclasses.dataclass(frozen=True)
class Projection:
    """The model inputs of one candidate, or the status that withholds them.

    The status is admitted, incomplete (a required role has no token), malformed (a selector failure) or unassembled (an
    assembly, tokenization or length failure).
    """

    task: str
    candidate: str
    status: str
    reason: str | None
    candidate_span: tuple[int, int] | None
    role_status: dict[str, str]
    encoding: tk.Encoding | None
    tokens: int
    facts: Rows | None
    mu: Rows | None
    masks: dict[str, Mask] | None

    def arm(self, name: str) -> tuple[Rows, Rows, Mask]:
        """Return the shared facts and `mu` with the mask of the named arm, if admitted."""
        if (
            self.status != "admitted"
            or self.facts is None
            or self.mu is None
            or self.masks is None
        ):
            raise StructureError(f"{self.candidate} is {self.status}; no inputs")
        return self.facts, self.mu, self.masks[name]


def additive_mask(membership: Sequence[Sequence[bool]]) -> Mask:
    """Return pool mask rows: 0.0 where a token is in the pool, -inf elsewhere."""
    return [[0.0 if member else -math.inf for member in row] for row in membership]


def _row(*values: str) -> list[int]:
    try:
        return [inv.index(v) for inv, v in zip(INVENTORIES, values, strict=True)]
    except ValueError as exc:
        raise StructureError(f"facts {values} outside the inventories") from exc


def _capped(value: int | None, cap: int) -> str:
    if value is None:
        return "None"
    return f"{cap}+" if value >= cap else str(value)


def _source_row(fact: Sequence[Any]) -> list[int]:
    _, _, kind, parent, slot, ordinal, last, depth, lit = fact
    return _row(
        kind,
        parent,
        slot or "None",
        _capped(ordinal, 8),
        last or "None",
        _capped(depth, 7),
        LITS[min(lit, 2)],
        "Source",
    )


SEAM = _row("Sep", "Sep", "None", "None", "None", "None", "None", "Sep")
FRAME = _row("NoStmt", "None", "None", "None", "None", "None", "None", "Frame")
PAD = _row("NoStmt", "None", "None", "None", "None", "None", "None", "Pad")


def _check_cover(cand: str, facts: Sequence[Sequence[Any]], runs: Any) -> None:
    index = 0
    for start, end in runs:
        at = start
        while index < len(facts) and facts[index][0] < end:
            if facts[index][0] != at or facts[index][1] <= at:
                raise StructureError(f"{cand}: run byte {at} is not covered exactly")
            at = facts[index][1]
            index += 1
        if at != end:
            raise StructureError(f"{cand}: run bytes from {at} are not covered")
    if index != len(facts):
        raise StructureError(f"{cand}: facts outside the runs; runs not covered")


def _unusable(task: str, cand: str, status: str, reason: str) -> Projection:
    return Projection(task, cand, status, reason, None, {}, None, 0, None, None, None)


def project(
    tokenizer: Any,
    source: bytes,
    result: Mapping[str, Any],
    *,
    max_tokens: int,
) -> Projection:
    """Compute the token facts, the role membership and the masks of both arms for one result."""
    task, cand = result["task"], result["cand"]
    if task not in ROLES:
        raise StructureError(f"{cand}: unknown task {task!r}")
    identity = result["source"]
    sha = hashlib.sha256(source).hexdigest()
    if identity["bytes"] != len(source) or identity["sha256"] != sha:
        raise StructureError(
            f"{cand}: source identity {len(source)} bytes {sha} does not match "
            f"{identity['bytes']} bytes {identity['sha256']}"
        )
    if not result["ok"]:
        return _unusable(task, cand, "malformed", result["reason"])
    facts = result["facts"]
    _check_cover(cand, facts, result["runs"])
    try:
        runs = tk.assemble(
            [tk.Piece("", sha, s, source[s:t]) for s, t in result["runs"]],
            [("", sha)],
            cand,
        )
        encoding = tk.encode(tokenizer, runs, cand)
    except tk.TokenizeError as exc:
        return _unusable(task, cand, "unassembled", str(exc))
    if encoding.total_tokens > max_tokens:
        reason = f"{encoding.total_tokens} framed tokens exceed {max_tokens}"
        return _unusable(task, cand, "unassembled", reason)

    names, roles = ROLES[task], result["roles"]
    starts = [fact[0] for fact in facts]
    last = len(encoding.tokens) - 1
    rows: Rows = []
    mu: Rows = []
    for index, token in enumerate(encoding.tokens):
        if token.end is None:
            framing = index < LEADING_FRAME or index == last
            rows.append(list(FRAME if framing else SEAM))
            mu.append([0] * len(names))
            continue
        byte = token.end - 1
        rows.append(_source_row(facts[bisect.bisect_right(starts, byte) - 1]))
        member = [any(s <= byte < t for s, t in roles[n]["iv"]) for n in names]
        mu.append([int(m) for m in member])

    role_status: dict[str, str] = {}
    for r, name in enumerate(names):
        if roles[name]["why"] == "structural_empty":
            role_status[name] = "structural_empty"
        elif any(row[r] for row in mu):
            role_status[name] = "present"
        else:
            role_status[name] = "no_token"
    missing = [name for name in REQUIRED[task] if role_status[name] != "present"]

    tokens = len(rows)
    source_tokens = [token.end is not None for token in encoding.tokens]
    masks = {
        "F": additive_mask([source_tokens] * len(names)),
        "S": additive_mask([[bool(row[r]) for row in mu] for r in range(len(names))]),
    }
    cand_iv = roles["CAND"]["iv"][0]
    return Projection(
        task=task,
        candidate=cand,
        status="incomplete" if missing else "admitted",
        reason=f"required roles without tokens: {missing}" if missing else None,
        candidate_span=(cand_iv[0], cand_iv[1]),
        role_status=role_status,
        encoding=encoding,
        tokens=tokens,
        facts=rows,
        mu=mu,
        masks=masks,
    )


def run_selector(
    command: Sequence[str],
    task: str,
    source: bytes,
    source_id: str,
    *,
    stderr: list[str] | None = None,
) -> list[dict[str, Any]]:
    """Run the selector command on one source and parse its result lines.

    A supplied `stderr` list receives the decoded stderr of the selector.
    """
    done = subprocess.run(
        [*command, "--task", task, "--id", source_id],
        input=source,
        capture_output=True,
        check=False,
    )
    if stderr is not None:
        stderr.append(done.stderr.decode("utf-8", errors="replace"))
    if done.returncode != 0:
        detail = done.stderr.decode("utf-8", errors="replace").strip()
        raise StructureError(f"selector exited {done.returncode}: {detail}")
    return [json.loads(line) for line in done.stdout.splitlines() if line.strip()]
