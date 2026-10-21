# Releasing STAVE

The release workflow builds and tests the assets, then creates a GitHub draft. You review and publish that draft.

Run the commands below from the repository root with the [development tools](../README.md#development) installed.
Examples use `0.1.1`; replace it with the version you intend to release.

## 1. Choose the version

Choose the version manually using [semantic versioning](https://semver.org/). Release tags must use
`vMAJOR.MINOR.PATCH`, with no prerelease or build suffix.

STAVE's compatibility policy covers documented CLI options, JSON/LSP contracts and editor settings. During `0.x`:

- Use a patch release for compatible fixes, including corrected diagnostics when these contracts remain compatible.
- Use a minor release for new features or breaking changes.

Publish `1.0.0` once these interfaces are stable.

## 2. Prepare the release commit

Set the same version in the extension and runtime. These commands also update their lock files; skip the first two
commands if the intended version is already set:

```sh
npm --prefix editors/vscode version 0.1.1 --no-git-tag-version
uv version --project infer 0.1.1 --no-sync
uv run --project infer --locked python scripts/release/version.py --check
```

The four version files are `editors/vscode/package.json`, `editors/vscode/package-lock.json`, `infer/pyproject.toml` and
`infer/uv.lock`. Their numeric version must match the tag without its `v` prefix. The workflow stamps the full tag
into the Go binary during the build.

If model bundles change, update their URLs and zip hashes in the [catalog](../internal/models/catalog.json) now.
The catalog is embedded in each binary. Follow [model bundle publishing](model-bundles.md#publish-and-catalog) for
the separate model release.

Run the [local checks](ci.md#local-checks), review the changes and commit them.

Push the release commit to the repository where you intend to publish. Replace `RELEASE_REMOTE` and `RELEASE_BRANCH`
with the remote and branch names; use `git remote -v` to check the destination.

```sh
git push RELEASE_REMOTE RELEASE_BRANCH
```

## 3. Rehearse on GitHub (optional)

Rehearse before the first release and after workflow or packaging changes. For routine releases, you can skip to
[step 4](#4-tag-the-release-commit): the tag-triggered run performs the same checks before creating a draft.

In that repository, open **Actions → Release → Run workflow**, select the pushed branch and start the run.
The workflow must already be on the repository's default branch for
[manual runs](https://docs.github.com/en/actions/how-tos/manage-workflow-runs/manually-run-a-workflow) to be available.

Wait for `checks`, `build` and every `verify` job to pass. The `release` job is skipped: manual runs keep the assets
in Actions and create no draft. Branch builds use `dev-<commit>` for the binary and runtime-source version.

Record the successful run's commit hash. If you change the source or release metadata, commit, push and rehearse again.

## 4. Tag the release commit

Use the successful rehearsal's commit hash, or the commit prepared and pushed in step 2 if you skipped rehearsal.
Replace `RELEASE_COMMIT` with that hash, and use the same remote as in step 2:

```sh
git tag -a v0.1.1 RELEASE_COMMIT -m "STAVE v0.1.1"
git push RELEASE_REMOTE v0.1.1
```

The tag push starts Release. It builds and verifies the tagged assets, then creates the draft only after
all checks pass.

If a run fails, inspect its logs before continuing. Rerun it after resolving a transient failure.
Reruns can refresh draft assets; the workflow refuses to replace a published release. After publication, release
fixes under a new version.

## 5. Review and publish

Open the draft on that repository's **Releases** page:

- Review the generated notes and describe any breaking changes.
- Confirm the tag, version and [expected assets](#release-assets).
- Download the model zips from the [catalog's URLs](../internal/models/catalog.json) and compare their SHA-256 hashes
  with the catalog. Workflow verification uses toy models and does not check these downloads.

Click **Publish release** when these checks are complete.

## Release assets

[build.sh](../scripts/release/build.sh) produces:

| Asset | Contents |
| --- | --- |
| `stave_<version>_<os>_<arch>.tar.gz` | Linux or macOS binary and license notices. |
| `stave_<version>_windows_amd64.zip` | Windows binary and license notices. |
| `stave-vscode-<version>.vsix` | Packaged VS Code extension. |
| `stave-infer_<version>.tar.gz` | Runtime source and lock file embedded in the binary, plus license notices. |
| `SHA256SUMS` | SHA-256 hashes of the assets above. |

Binary archives include `LICENSE`, `NOTICE` and `GO-LICENSE`. The VSIX includes STAVE's license and
`THIRD_PARTY_NOTICES.txt` for its bundled dependencies. The build checks these files and the extension's helper for
stopping the server before generating checksums.

Binary and runtime-source asset versions include the tag's `v`; the VSIX uses the numeric package version.
[Model bundles](model-bundles.md#publish-and-catalog) are published separately and retain their own ids and versions.
