INFER := uv run --directory infer --locked
VSCODE := npm --prefix editors/vscode
TOY := $(CURDIR)/build/toy
# The version that stave version and the language server report; release builds supply the tag.
VERSION ?= dev-$(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)

.PHONY: test test-go test-infer check-infer wheel build toy editor-test vsix
test: test-go test-infer

test-go:
	go vet ./...
	go test -count=1 ./...

test-infer:
	$(INFER) pytest

check-infer:
	$(INFER) ruff check
	$(INFER) ruff format --check
	$(INFER) pyright

wheel:
	uv build --directory infer --wheel

build:
	go build -ldflags "-X github.com/aholovko/stave/internal/app.Version=$(VERSION)" -o build/stave ./cmd/stave

# Two toy bundles let editor tests exercise model switching.
toy:
	$(INFER) python -m stave_infer.toy --variant S --out $(TOY)/toy-S --tokenizer-dir tests/fixtures/tokenizer
	$(INFER) python -m stave_infer.toy --variant B --out $(TOY)/toy-B --tokenizer-dir tests/fixtures/tokenizer

# Opens a VS Code window; without STAVE_VSCODE_EXECUTABLE a stable VS Code is downloaded first.
editor-test: build toy
	STAVE_SERVER_PATH=$(CURDIR)/build/stave \
	STAVE_RUNTIME_PYTHON="$$($(INFER) python -c 'import sys; print(sys.executable)')" \
	STAVE_EDITOR_BUNDLE=$(TOY)/toy-S \
	STAVE_EDITOR_BUNDLE_B=$(TOY)/toy-B \
	$(VSCODE) test

vsix:
	$(VSCODE) run package
