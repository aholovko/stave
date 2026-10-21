# STAVE for VS Code

STAVE diagnostics for Go packages, shown in the editor when you open or save a file.

## Installation

Requires VS Code 1.82 or newer.

1. [Install and set up STAVE](https://github.com/aholovko/stave#installation), then check it with `stave doctor`.
2. Download `stave-vscode-<version>.vsix` from the same release. In VS Code, run **Extensions: Install from VSIX…** from
   the Command Palette and select the file.
3. Open a Go package in a trusted workspace, then open or save a Go file.

The extension starts `stave lsp --stdio`. If `stave` is not on VS Code's `PATH`, set `stave.serverPath` below.

## Using STAVE

STAVE analyses the saved package. Editing a file clears its diagnostics until you save it; changes to other Go files in
the package also trigger analysis. Exact findings appear as warnings and advisory (model) findings as information.

The **STAVE** status bar item describes the active Go file. Hover for findings, coverage and model details; click to
open the **STAVE** output channel.

| Status | Meaning |
|--------|---------|
| `starting` / `pending` | Waiting for the server or this file's analysis. |
| `running` | Exact results are ready; model analysis is still running. |
| `complete` | Analysis has finished. Check findings and coverage. |
| `buffer differs from the saved file` | Save the file to analyse it. |
| `not analysed` | The file is outside the analysis targets; the reason is shown. |
| `failed` | Reading the file, analysis or model inference failed; the reason is shown. |
| `server not running` | Check the output channel and `stave.serverPath`. |

`not judged` means a class or candidate was not checked for this file. A `complete` status alone does not mean the file
is clean. If the buffer mismatch remains after saving, check the file's encoding or byte order mark.

To switch models, run `stave model use ID`. The next save uses the selected model without restarting VS Code.

## Settings

| Setting | Default | Purpose |
|---------|---------|---------|
| `stave.serverPath` | `"stave"` | STAVE executable name or full path. |
| `stave.serverArgs` | `["lsp", "--stdio"]` | Server arguments: `lsp`, optional flags, then `--stdio`. |

Changing either setting restarts the server. Run `stave lsp -h` for available flags.

## Development

With Node.js 22 or newer, run these commands from the repository root:

```sh
npm --prefix editors/vscode ci --ignore-scripts
npm --prefix editors/vscode run test:unit
npm --prefix editors/vscode run package
```

Unit tests run without opening VS Code. Packaging bundles the client into `out/extension.js`, includes its Unix
shutdown helper and `THIRD_PARTY_NOTICES.txt`, and writes `dist/stave-vscode-<version>.vsix`.

With the [source-build prerequisites](https://github.com/aholovko/stave#development) installed, run `make editor-test`
to build STAVE and toy models and test diagnostics, saves, package changes and model switching in an isolated VS Code
profile. The runner downloads VS Code if needed.

To test a packaged extension using the same toy models, replace `<version>` with the version just built.
On Windows, first run `go build -o build/stave.exe ./cmd/stave` to create the filename expected by the runner.

```sh
npm --prefix editors/vscode test -- --vsix "../../dist/stave-vscode-<version>.vsix"
```

<details>
<summary>Integration test options and logs</summary>

Override these variables when running `npm --prefix editors/vscode test`:

| Variable | Default / purpose |
|----------|-------------------|
| `STAVE_SERVER_PATH` | `build/stave` (`stave.exe` on Windows). |
| `STAVE_RUNTIME_PYTHON` | The `infer/.venv` interpreter with `stave_infer` installed. |
| `STAVE_EDITOR_BUNDLE` | `build/toy/toy-S`; accepts a zip or directory. |
| `STAVE_EDITOR_BUNDLE_B` | `build/toy/toy-B`; set empty to skip model switching. |
| `STAVE_VSCODE_EXECUTABLE` | Downloaded VS Code; set to use an existing executable. |
| `STAVE_EDITOR_TEST_ROOT` | OS temporary directory; use a short path to avoid Unix socket limits. |
| `STAVE_EDITOR_TIMEOUT_MS` | `300000` (five minutes). |

Each run prints and keeps its directory, including `vscode.log`, model installation logs, `receipt.json` with statuses
and diagnostics, and `runner.json` with the result. Failed runs also print the editor log. Relative `--vsix` paths
resolve from `editors/vscode/`.

</details>
