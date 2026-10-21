"""`python -m stave_infer serve | check-bundle | version`.

`serve` handles one stdin/stdout exchange without network access. `check-bundle` is the compatibility check
used during installation. Stdout is reserved for results; handled runtime failures go to stderr with exit status 1.
"""

import argparse
import importlib.metadata
import json
import os
import sys
from collections.abc import Sequence
from pathlib import Path

# Set before importing Transformers so analysis cannot trigger downloads.
os.environ["HF_HUB_OFFLINE"] = "1"
os.environ["TRANSFORMERS_OFFLINE"] = "1"

from stave_infer import backend, bundle, request, serving  # noqa: E402

PROG = "stave_infer"


def _positive(text: str) -> int:
    value = int(text)
    if value < 1:
        raise argparse.ArgumentTypeError(f"{value} is not a positive integer")
    return value


def _parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog=f"python -m {PROG}")
    commands = parser.add_subparsers(dest="command", required=True)
    serve = commands.add_parser("serve", help="answer one request on stdin")
    serve.add_argument("--bundle", type=Path, required=True)
    serve.add_argument("--selector", required=True, help="the stave executable")
    serve.add_argument("--threads", type=_positive)
    serve.add_argument("--trace-dir", type=Path)
    check = commands.add_parser("check-bundle", help="verify and load one bundle")
    check.add_argument("dir", type=Path)
    commands.add_parser("version", help="print the runtime version")
    return parser


def version() -> str:
    formats = " ".join(bundle.SUPPORTED_MODEL_FORMATS)
    return (
        f"stave-infer {importlib.metadata.version('stave-infer')} "
        f"(bundle {bundle.FORMAT}; models {formats}; protocol {request.SCHEMA})"
    )


def _serve(args: argparse.Namespace) -> None:
    payload = sys.stdin.buffer.read()
    request.parse_request(payload)
    manifest = bundle.read_manifest(args.bundle)
    loaded = serving.load(args.bundle, manifest, threads=args.threads)
    response = backend.respond(
        payload,
        loaded=loaded,
        manifest=manifest,
        selector=[args.selector, "structural"],
        trace_dir=args.trace_dir,
    )
    sys.stdout.write(json.dumps(response, allow_nan=False) + "\n")


def _check(args: argparse.Namespace) -> None:
    manifest = bundle.read_manifest(args.dir)
    serving.load(args.dir, manifest, threads=None)
    print(
        f"{manifest.id}: compatible (variant {manifest.variant}, {manifest.model.format}, "
        f"model sha256 {manifest.model.sha256})"
    )


def main(argv: Sequence[str] | None = None) -> int:
    args = _parser().parse_args(argv)
    try:
        if args.command == "serve":
            _serve(args)
        elif args.command == "check-bundle":
            _check(args)
        else:
            print(version())
    except (ValueError, OSError, RuntimeError) as exc:
        print(f"{PROG}: {' '.join(str(exc).split())}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
