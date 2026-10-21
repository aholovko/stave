# Model bundles

A bundle contains a trained STAVE model, its backbone configuration, tokenizer and inference settings. It installs
under `<data>/models/<id>/`; see the [README](../README.md#update-and-uninstall) for the data directory.

## Install and select

```sh
stave model add PATH/TO/BUNDLE.zip
stave model use BUNDLE_ID
stave doctor
```

Replace `BUNDLE_ID` with the id printed by `model add`. You can also add a bundle directory, download URL or published
catalog id. The first installed model becomes active automatically. Use `stave model list` to see installed models.

New installations run the [compatibility check](#compatibility). Failed installations are removed; matching installed
bundles are reused. Different bundles cannot share an installed id. Use a new id or remove the old inactive model first.

`stave doctor` rechecks the active bundle. If a catalog entry reports “not published yet”, install a local zip or
directory.

## Bundle format

The packaging tool produces a zip with one top-level directory:

```text
S-20261020/
  manifest.json
  model.pt
  backbone-config.json
  tokenizer/
    vocab.json
    merges.txt
    tokenizer_config.json
    special_tokens_map.json
    config.json
  LICENSE
  NOTICE
```

The tokenizer directory requires these five files; only `.DS_Store` and `Thumbs.db` are allowed as extras.
Paths must be relative and stay inside the bundle. Archives may contain only regular files and directories.

`manifest.json` uses `stave-model-bundle-1`:

| Field | Contract |
| --- | --- |
| `id` | Letters, digits, `.`, `_` and `-`; starts with a letter or digit. |
| `variant`, `model.format` | B uses `stave-flat-1`; F and S use `stave-structural-6`. |
| `tasks` | Exactly `["P1", "P2", "P3", "TF", "DC", "RP"]`, in this order. |
| `seed`, `batch_size` | Nonnegative seed and positive batch size, matching the training and evaluation setup. |
| `thresholds` | A finite raw-logit threshold for each task. |
| `input_budgets` | An integer from 1 to 1,023 for each task, including framing tokens. |
| `model`, `backbone`, `tokenizer` | File paths and SHA-256 hashes; `model.bytes` also pins the model's size. |
| `provenance` | An object recording training origin and any task-ID conversion. |

A raw logit at or above its threshold produces advice; the reported score is that logit. Candidates longer than their
input budget are reported as not judged.

The backbone section records the pretrained weights' identity, size and parameter count. Those original weights are
not included or downloaded: the runtime builds the backbone from its configuration and loads all tensors from
`model.pt`.

<details>
<summary>Manifest example</summary>

Values containing `…` are abbreviated, so this example is not a usable manifest. All top-level fields and the fields
shown in `model`, `backbone` and `tokenizer` are required. Unknown fields are allowed only inside `provenance`.
Hashes must be 64 lowercase hexadecimal characters.

```json
{
  "format": "stave-model-bundle-1",
  "id": "S-20261020",
  "variant": "S",
  "version": "2026.10.20",
  "description": "STAVE variant S",
  "tasks": ["P1", "P2", "P3", "TF", "DC", "RP"],
  "seed": 20261004,
  "thresholds": {"P1": 0.0, "P2": 0.0, "P3": 0.0, "TF": 0.0, "DC": 0.0, "RP": 0.0},
  "batch_size": 2,
  "input_budgets": {"P1": 1023, "P2": 1023, "P3": 1023, "TF": 512, "DC": 512, "RP": 512},
  "model": {"file": "model.pt", "format": "stave-structural-6", "sha256": "471a27c6…", "bytes": 520843587},
  "backbone": {
    "name": "microsoft/unixcoder-base",
    "revision": "5604afdc…",
    "config": "backbone-config.json",
    "config_sha256": "a7c07558…",
    "weight_sha256": "6732b600…",
    "weight_bytes": 503809527,
    "parameters": 125929728
  },
  "tokenizer": {
    "dir": "tokenizer",
    "revision": "5604afdc…",
    "files": {
      "vocab.json": "…",
      "merges.txt": "…",
      "tokenizer_config.json": "…",
      "special_tokens_map.json": "…",
      "config.json": "…"
    }
  },
  "provenance": {
    "run": "six-class-final-bfs-training-evaluation-2026-10-10/execution-001",
    "plan_sha256": "cf0dbe48…",
    "source_model_sha256": "a547ff1c…",
    "source_tasks": ["E1", "E2", "E5", "TF", "DC", "RP"],
    "conversion": "stave-task-ids-e-to-p-1"
  },
  "license": "Apache-2.0"
}
```

</details>

## Compatibility

`stave model add` runs the runtime's compatibility check for new installations. To check a directory from the
repository root:

```sh
uv run --project infer --locked python -m stave_infer check-bundle PATH/TO/BUNDLE
```

The check verifies pinned files, loads the tokenizer and model, and compares the saved configuration with the rebuilt
model. Architecture, backbone identity, tasks, seed, tensor names, dtypes and shapes must match. B treats `init.sha256`
as training provenance: initialization can differ between CPU architectures, and converted models retain the original
fingerprint. All trained tensors are still loaded strictly. Errors identify the incompatible field or file.
Analysis also verifies the manifest and pinned files before loading. The runtime accepts only P-based task inventories;
convert E-based research models with the packaging tool below.

A new training run can ship as a new bundle if it passes these checks. Changes to the architecture, tokenizer or input
contract that the runtime does not support require a new model format and a STAVE runtime release.

## Build from a training run

Run [make_bundle.py](../tools/bundle/make_bundle.py) from the repository root:

```sh
uv run --project infer --locked python tools/bundle/make_bundle.py \
  --run RUN_DIR --arm S --id S-20261020 --version 2026.10.20 \
  --backbone-config PATH/config.json \
  --tokenizer-dir TOKENIZER_DIR --tokenizer-receipt PATH/tokenizer-artifacts.json \
  --out OUT_DIR
```

Choose `B`, `F` or `S` for `--arm`; optionally add `--description "TEXT"`.

| Input | Required contents |
| --- | --- |
| `RUN_DIR` | `receipt.json`, `plan.json` and `terminal/<arm>/model.pt`. |
| `plan.json` | `arms` containing the selected arm, plus `tasks`, `thresholds`, `init_seed` and `eval_batch_size`. |
| Receipt's `expected` | Backbone configuration hash, pretrained weight hash and size, and parameter count. |
| `--backbone-config` | The pretrained model's `config.json`, matching the receipt's hash. |
| Tokenizer directory and receipt | The five tokenizer files with matching sizes and hashes. |

Both the run and selected arm must be complete. The run receipt must pin the plan and model hashes; the tokenizer
receipt supplies the model name and revision.

The tool accepts P1/P2/P3 models directly. For research runs using E1/E2/E5, it verifies the original receipts, then
converts task names in the checkpoint and configuration to P1/P2/P3. Tensor values stay unchanged; no retraining is
needed. The manifest records the source checkpoint hash and conversion in `provenance`, and pins the converted file.
B's saved initialization fingerprint still describes the original training initialization, including its E-based names.
Research files and receipts stay unchanged. Use a new bundle ID when converting an existing model.

The date in `S-20261020` and version `2026.10.20` records the bundle revision. Use the same date for B, F and S packaged
together; retain the original training dates in `provenance`.

The tool adds `LICENSE` and `NOTICE` and writes the manifest. It sets token budgets to 1,023 for
P1/P2/P3 and 512 for TF/DC/RP. It writes `OUT_DIR/<id>/` and `OUT_DIR/<id>.zip`, refuses existing outputs, and prints
the zip's size and SHA-256. With the locked inference environment, identical inputs produce identical zip bytes.

Packaging verifies file hashes. Install the zip for the runtime’s full [compatibility check](#compatibility):

```sh
stave model add OUT_DIR/S-20261020.zip
```

For converted research models, retain JSON reports from these checks before publishing:

- [Loader parity](../tools/parity/loader_parity.py) compares B/F/S tensors and predictions with the original run. Use
  the locked research environment, original pretrained model directory and research selector, and current STAVE binary.
  Require each variant's `ok` field to be true; this includes equal reports and zero errors from both loaders.
- [Case-study parity](../tools/parity/case_study_parity.py) checks the converted S model against the retained case-study
  evidence. Use the locked inference environment and a separate data directory with S installed; it changes the thread
  setting and needs network access to clone the frozen source repositories.
  Require the report's top-level `ok` field to be true.

Each script's `--help` shows the command and required arguments.

Bundles are about 480 MB zipped; keep them out of Git.

## Publish and catalog

[`internal/models/catalog.json`](../internal/models/catalog.json) is the authoritative list of bundle ids, variants,
download URLs and zip hashes. It also names the default model installed by `stave setup`. The catalog is embedded in
each binary; `stave model add ID` verifies the downloaded zip against its catalog hash.

Catalog URLs can be prepared before upload and become usable after the model release is published. Until publication,
install the local zip.

To publish a bundle:

1. Build it, pass the compatibility check and retain any conversion parity reports.
2. Prepare its id, variant, planned URL and zip SHA-256 in the catalog.
3. Upload the zip to a separate model release, such as `models-20261020`, in
   [GitHub Releases](https://github.com/aholovko/stave/releases). The release tag must match the tag in the catalog URL.
   Do not mark it as the latest release; with `gh`, use `--latest=false`.
4. Publish the model release, then download each catalog URL and verify its zip hash before publishing the binary
   release.

Upload the exact zip that passed validation. Changing any bundled file, including `LICENSE` or `NOTICE`, changes its
hash. Give changed published bundles a new ID.

An existing binary can install a compatible new bundle by URL or local path before its catalog is updated.
