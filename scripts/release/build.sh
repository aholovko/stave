#!/usr/bin/env bash
# Build and check release assets consistently for local and CI runs. See docs/releasing.md for the asset list.
# Usage: bash scripts/release/build.sh VERSION OUT_DIR
# Requires Go, Node.js/npm, Python 3, tar, zip and shasum. OUT_DIR must be absent or empty.
set -euo pipefail
# Keep macOS metadata out of release archives.
export COPYFILE_DISABLE=1

if [ $# -ne 2 ]; then
  echo "usage: bash scripts/release/build.sh VERSION OUT_DIR" >&2
  exit 2
fi
version=$1
root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
mkdir -p "$2"
out=$(cd "$2" && pwd)
if [ -n "$(ls -A "$out")" ]; then
  echo "build.sh: $out is not empty" >&2
  exit 1
fi
stage=$(mktemp -d)
trap 'rm -rf "$stage"' EXIT

for target in darwin/arm64 linux/amd64 linux/arm64 windows/amd64; do
  goos=${target%/*}
  goarch=${target#*/}
  name=stave_${version}_${goos}_${goarch}
  exe=stave
  if [ "$goos" = windows ]; then exe=stave.exe; fi
  mkdir "$stage/$name"
  (cd "$root" && CGO_ENABLED=0 GOOS=$goos GOARCH=$goarch go build -trimpath \
    -ldflags "-s -w -X github.com/aholovko/stave/internal/app.Version=$version" -o "$stage/$name/$exe" ./cmd/stave)
  cp "$root/LICENSE" "$root/NOTICE" "$stage/$name/"
  cp "$root/third_party/go/LICENSE" "$stage/$name/GO-LICENSE"
  if [ "$goos" = windows ]; then
    (cd "$stage" && zip -q -r "$out/$name.zip" "$name")
  else
    tar -czf "$out/$name.tar.gz" -C "$stage" "$name"
  fi
done

npm --prefix "$root/editors/vscode" ci --ignore-scripts
npm --prefix "$root/editors/vscode" run package
vsix=stave-vscode-$(node -p "require(process.argv[1]).version" "$root/editors/vscode/package.json").vsix
cp "$root/dist/$vsix" "$out/$vsix"

tar -czf "$out/stave-infer_$version.tar.gz" -C "$root" --exclude __pycache__ \
  LICENSE NOTICE infer/pyproject.toml infer/uv.lock infer/.python-version infer/src/stave_infer

python3 "$root/scripts/release/artifacts.py" "$version" "$out"

(cd "$out" && shasum -a 256 -- * >"$stage/SHA256SUMS")
mv "$stage/SHA256SUMS" "$out/SHA256SUMS"
ls -l "$out"
