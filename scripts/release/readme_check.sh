#!/usr/bin/env bash
# Run README Bash blocks in order with a fresh STAVE_HOME and working directory to protect the user's installation.
# A supplied bundle replaces the default download, allowing checks before model publication.
# Failed runs retain the sandbox for diagnosis; successful runs remove it.
#
# Usage: bash scripts/release/readme_check.sh [--stave FILE] [--bundle ZIP|DIR] [--readme FILE]
# STAVE_BIN and STAVE_README_BUNDLE provide defaults for --stave and --bundle.
# Supports Bash on macOS/Linux and Git Bash on Windows.
set -euo pipefail

usage() {
  echo "usage: bash scripts/release/readme_check.sh [--stave FILE] [--bundle ZIP|DIR] [--readme FILE]" >&2
  exit 2
}

stave_bin=${STAVE_BIN:-}
bundle=${STAVE_README_BUNDLE:-}
readme="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)/README.md"
while [ $# -gt 0 ]; do
  [ $# -ge 2 ] || usage
  case $1 in
    --stave) stave_bin=$2 ;;
    --bundle) bundle=$2 ;;
    --readme) readme=$2 ;;
    *) usage ;;
  esac
  shift 2
done

fail() {
  echo "readme_check: $*" >&2
  exit 1
}

# Resolve paths before the working directory changes.
absolute() {
  echo "$(cd "$(dirname "$1")" && pwd)/$(basename "$1")"
}

# Git Bash does not convert every environment variable, so native Windows programs
# need paths converted explicitly.
native() {
  if command -v cygpath >/dev/null 2>&1; then cygpath -w "$1"; else echo "$1"; fi
}

[ -f "$readme" ] || fail "no README at $readme"
readme=$(absolute "$readme")

sandbox=$(mktemp -d)
mkdir -p "$sandbox/bin" "$sandbox/home" "$sandbox/work"
if [ -n "$stave_bin" ]; then
  [ -f "$stave_bin" ] || fail "no stave binary at $stave_bin"
  # A copied download may have lost its executable bit.
  case $stave_bin in
    *.exe) cp "$stave_bin" "$sandbox/bin/stave.exe" ;;
    *) cp "$stave_bin" "$sandbox/bin/stave" && chmod +x "$sandbox/bin/stave" ;;
  esac
fi
export PATH="$sandbox/bin:$PATH"
command -v stave >/dev/null 2>&1 || fail "stave is not on PATH; set STAVE_BIN or pass --stave"
export STAVE_HOME
STAVE_HOME=$(native "$sandbox/home")

script="$sandbox/readme.sh"
setup_line=
if [ -n "$bundle" ]; then
  [ -e "$bundle" ] || fail "no bundle at $bundle"
  setup_line="stave setup -model $(printf '%q' "$(native "$(absolute "$bundle")")")"
fi
# README line markers make a failed example easy to locate.
# Pass paths through the environment; awk -v would treat Windows backslashes as escapes.
README="$readme" SETUP="$setup_line" awk '
  BEGIN { readme = ENVIRON["README"]; setup = ENVIRON["SETUP"]; print "set -euo pipefail"; blocks = 0; setups = 0 }
  !inside && /^```bash[ \t]*$/ { inside = 1; blocks++; printf "echo \"readme_check: block %d, %s:%d\" >&2\n", blocks, readme, NR; next }
  inside && /^```[ \t]*$/ { inside = 0; next }
  inside && setup != "" && $0 ~ /^stave setup[ \t]*$/ { print setup; setups++; next }
  inside { print }
  END {
    if (inside) { print "README: unterminated bash block" > "/dev/stderr"; exit 1 }
    if (blocks == 0) { print "README: no bash blocks" > "/dev/stderr"; exit 1 }
    if (setup != "" && setups != 1) { printf "README: %d `stave setup` lines, want 1 to replace\n", setups > "/dev/stderr"; exit 1 }
  }
' "$readme" >"$script" || {
  rm -rf "$sandbox"
  fail "cannot extract the bash blocks of $readme"
}

echo "readme_check: running the bash blocks of $readme in $sandbox/work with $(command -v stave)" >&2
if (cd "$sandbox/work" && bash -x "$script"); then
  rm -rf "$sandbox"
  echo "readme_check: PASS" >&2
else
  fail "FAIL; the sandbox is kept in $sandbox"
fi
