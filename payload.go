// Package stave embeds the inference runtime so the binary and runtime stay in sync.
package stave

import "embed"

// Payload contains the runtime sources and dependency lock file under their repository paths.
//
//go:embed infer/pyproject.toml infer/uv.lock infer/.python-version infer/src/stave_infer/*.py
var Payload embed.FS
