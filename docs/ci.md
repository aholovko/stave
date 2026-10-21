# Continuous integration

[CI](../.github/workflows/ci.yml) runs on branch pushes and pull requests, on Linux, macOS and Windows.

| Area | Checks |
| --- | --- |
| Go | Formatting, `go vet` and tests; race detection on Linux only. |
| Python | Ruff, Pyright and pytest, including end-to-end CLI tests, with locked dependencies. |
| VS Code | Extension unit tests. |
| Workflows | Workflow validation with actionlint, on Linux only. |
| Release tooling | Version consistency, version-validation tests and packaging-check tests. |

## Release verification

[Release](../.github/workflows/release.yml) runs the same CI checks and tests packaged assets on every
[supported system](../README.md#supported-systems). It checks required package files and hashes, installs the runtime,
runs CLI and VS Code integration tests, and replays the README's Bash examples using toy models.

See [Releasing STAVE](releasing.md) for the release steps and asset list.

## Local checks

Start with [Development](../README.md#development), including `make editor-test` for VS Code integration tests.
Additional CI checks:

```sh
go run github.com/rhysd/actionlint/cmd/actionlint@v1.7.12
go test -race -count=1 ./...
uv run --project infer --locked python -m unittest discover -s scripts/release -p "test_*.py"
```

For the same shell checks as Linux CI, install ShellCheck and make `shellcheck` available on `PATH`.

To replay the README's Bash examples with a local model bundle:

```sh
bash scripts/release/readme_check.sh --stave build/stave --bundle PATH/TO/BUNDLE.zip
```

The script uses a temporary installation and substitutes the supplied bundle for the default model download.
