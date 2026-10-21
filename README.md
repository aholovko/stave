# STAVE

STAVE (STructural-Automaton Violation Encoder) finds style issues in Go code. It combines rule-based checks with model
suggestions and runs entirely locally after setup. You can use it from the command line and in VS Code or other editors
through the Language Server Protocol (LSP).

## Supported classes

| Class | Checks for | Analysis |
| --- | --- | --- |
| P1 | Unnecessary `else` after a direct `return` | Rule + model |
| P2 | Error strings with capitalization or trailing punctuation | Rule + model |
| P3 | Underscores in declared names | Rule + model |
| TF | Test failures that lack useful diagnostic details | Model |
| DC | Documentation comments on exported structs and methods | Model |
| RP | Unclear unnamed results with repeated types in exported signatures | Model |

Rule-based findings are labelled `exact`; model suggestions are labelled `advisory` and include a score. The report also
lists classes that were not judged and explains why. STAVE analyses saved files and does not change your code.

## Supported systems

- macOS 14+ on Apple Silicon
- Linux on x86_64 or arm64, with glibc 2.28+
- Windows 10 or 11 on x86_64

Allow 8 GB of RAM and about 2.5 GB of disk space for the runtime and one model. Inference uses the CPU.

## Prerequisites

- [uv](https://docs.astral.sh/uv/getting-started/installation/) to install the runtime.
- [VS Code](https://code.visualstudio.com/) 1.82+ for VS Code integration.

`stave setup` installs Python and the inference dependencies for you. A Go toolchain is not required to run STAVE.

## Installation

### 1. Install the command-line tool

Download the archive for your system from [Releases](https://github.com/aholovko/stave/releases), extract it, and put
`stave` (or `stave.exe`) on your `PATH`. The commands below use `v0.1.0`; change the version to match your download.

<details>
<summary>macOS and Linux</summary>

Set `TARGET` to `darwin_arm64` for macOS, `linux_amd64` for Linux x86_64, or `linux_arm64` for Linux arm64.

```sh
VERSION=v0.1.0
TARGET=darwin_arm64
curl -fLO "https://github.com/aholovko/stave/releases/download/$VERSION/stave_${VERSION}_${TARGET}.tar.gz"
tar -xzf "stave_${VERSION}_${TARGET}.tar.gz"
mkdir -p ~/.local/bin
mv "stave_${VERSION}_${TARGET}/stave" ~/.local/bin/
```

Make sure `~/.local/bin` is on your `PATH`.

</details>

<details>
<summary>Windows (PowerShell)</summary>

```powershell
$Version = "v0.1.0"
Invoke-WebRequest -OutFile stave.zip `
  "https://github.com/aholovko/stave/releases/download/$Version/stave_${Version}_windows_amd64.zip"
Expand-Archive stave.zip -DestinationPath .
$Bin = "$env:LOCALAPPDATA\Programs\stave"
New-Item -ItemType Directory -Force $Bin | Out-Null
Move-Item -Force "stave_${Version}_windows_amd64\stave.exe" $Bin
[Environment]::SetEnvironmentVariable("Path", [Environment]::GetEnvironmentVariable("Path", "User") + ";$Bin", "User")
```

Open a new terminal so the `Path` change applies.

</details>

### 2. Set up the runtime and model

```bash
stave setup
stave doctor
```

Setup installs the default model, `S-20261020`, and downloads about 1.1 GB on the first run. `stave doctor` checks the
installation and explains any failures. Analysis works offline after setup.

### 3. Add VS Code integration (optional)

Download `stave-vscode-<version>.vsix` from the same release. In VS Code, run **Extensions: Install from VSIX…** from
the command palette and select the file.

Open a trusted folder containing Go files. Findings appear when you open or save a file. Hover over the `STAVE` status
bar item for findings and coverage; click it to open the output log. See the [extension guide](editors/vscode/README.md)
for settings and status meanings.

### Other LSP editors

Configure `stave lsp --stdio` as your editor's language server for Go files. Diagnostics use standard LSP; the VS Code
extension additionally shows STAVE-specific analysis status and coverage.

## Try it

Run in a shell (Bash or Git Bash on Windows):

```bash
mkdir demo
cat > demo/demo.go <<'EOF'
package demo

import "errors"

var ErrBad = errors.New("Bad.")

func Pick(ok bool) int {
	if ok {
		return 1
	} else {
		return 2
	}
}

func set(p_value int) int { return p_value }

// stores things.
type Cache struct{}

func Bounds() (int, int) { return 0, 100 }
EOF
cat > demo/demo_test.go <<'EOF'
package demo

import "testing"

func TestPick(t *testing.T) {
	if got := Pick(true); got != 2 {
		t.Error("wrong result")
	}
}
EOF
stave check demo
```

With `S-20261020`, the findings include the following (scores may vary slightly):

```text
demo/demo.go:5:25: P2 exact: error-literal form
demo/demo.go:10:4: P1 exact: else after direct return
demo/demo.go:15:10: P3 exact: underscore at a declaring position
demo/demo.go:17:1: DC advisory: documentation comments [S-20261020 score=9.21]
demo/demo.go:20:15: RP advisory: result-parameter clarity [S-20261020 score=2.23]
demo/demo_test.go:7:3: TF advisory: missing diagnostic details in test failures [S-20261020 score=5.45]
```

The full output also includes model suggestions for P1–P3, candidate counts and coverage. TF applies only to the test
file; the other five classes apply to `demo.go`. Open the `demo` folder in VS Code to see the same findings in the
editor.

## Common commands

```bash
stave version                        # show binary, runtime and model versions
stave check -json demo > demo.json    # save a JSON report
stave check -advisory disabled demo   # run rule-based checks only
stave check -tests=false demo         # skip test files
```

Use `stave check -h` for all options, including build tags and target platform. Findings do not cause a nonzero exit
code; command or analysis failures do.

## Models

The default model is ready after `stave setup`. To try another model:

```sh
stave model list
stave model add B-20261020
stave model use B-20261020
```

| Model | Description |
| --- | --- |
| `S-20261020` (default) | Uses code structure and pools tokens by structural role before scoring. |
| `B-20261020` | Uses a flat view of the source, without structural roles. |
| `F-20261020` | Uses code structure, but pools source tokens together when scoring. |

The next CLI run or VS Code save uses the selected model. Use `stave check -model ID demo` to choose a model for one
run, or `stave model remove ID` to remove an inactive model. `stave model add` also accepts a bundle URL, zip file or
directory. See [model bundles](docs/model-bundles.md) for formats and compatibility.

## Troubleshooting

Start with `stave doctor`.

| Problem | What to do |
| --- | --- |
| `uv is not on PATH` | Install uv, open a new terminal and rerun `stave setup`. |
| No runtime or model installed | Run `stave setup`, then `stave doctor`. |
| Model suggestions take time | Each check reloads the model; wait while `running` (default timeout: 180 seconds). |
| `buffer differs from the saved file` | Save the file; if it persists, use UTF-8 without a byte order mark. |
| VS Code shows `server not running` | Set `stave.serverPath` to the absolute path of the binary. |
| macOS blocks the unsigned binary | For a trusted download, run `xattr -d com.apple.quarantine ~/.local/bin/stave`. |
| Windows blocks the unsigned binary | For a trusted download, use the PowerShell command below. |

```powershell
Unblock-File "$env:LOCALAPPDATA\Programs\stave\stave.exe"
```

## Update and uninstall

To update, replace the binary and extension, run `stave setup`, then run **Developer: Reload Window** in VS Code. Setup
replaces the old runtime and keeps installed models.

To remove STAVE's runtime, models and configuration:

```bash
stave uninstall -yes
```

Then delete the binary and uninstall the extension in VS Code. uv and its shared Python installations and cache are
kept.

The default data directory is `~/Library/Application Support/stave` on macOS, `~/.local/share/stave` on Linux
(`$XDG_DATA_HOME/stave` when set), and `%LOCALAPPDATA%\stave` on Windows. Set `STAVE_HOME` to use another location. See
[Architecture](ARCHITECTURE.md#configuration-and-models) for configuration details.

## Development

Requires Go 1.26.10+, uv and Node.js 22+.

```sh
npm --prefix editors/vscode ci --ignore-scripts
make test          # Go vet, Go tests and Python tests
make check-infer   # Python linting and type checks
make build         # build/stave
make vsix          # dist/stave-vscode-<version>.vsix
make editor-test   # VS Code integration tests
```

After building, try the P1–P3 exact checks without an inference runtime or model:

```sh
build/stave check -advisory disabled path/to/package
```

For model suggestions, run `build/stave setup` to install the runtime and default model. To use a local bundle, add
`-model PATH/TO/BUNDLE.zip`.

See [Architecture](ARCHITECTURE.md), [CI](docs/ci.md), [Releasing STAVE](docs/releasing.md), and
[extension development](editors/vscode/README.md#development).

## Citation

See [CITATION.cff](CITATION.cff) for citation metadata and the related dissertation.
