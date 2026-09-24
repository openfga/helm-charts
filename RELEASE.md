# Release guide

Releases are cut with [release-please](https://github.com/googleapis/release-please), driven by a `workflow_dispatch` GitHub Actions workflow. This document covers how to run it from the GitHub UI.

## Versioning

The charts are pre-1.0.0, so semver is relaxed:

| Change          | Bump                | Example           |
| --------------- | ------------------- | ----------------- |
| Breaking change | **Minor** (`0.x.0`) | `0.4.0` → `0.5.0` |
| Everything else | **Patch** (`0.0.x`) | `0.4.0` → `0.4.1` |

`1.0.0` is a deliberate stability decision, not the result of a routine breaking change.

## Cutting a release

1. Go to **Actions → release-please → Run workflow**.
2. Choose a **bump-type**:
   - `auto` — let the Conventional Commits history decide the bump (the default).
   - `patch` / `minor` / `major` — force the bump.
   - `explicit` — set the exact version in **release-version** (e.g. `0.5.0` or `0.5.0-beta.1`).
3. The workflow opens a release PR titled `release: v<version>` that updates the changelog and chart version. Review and merge it.
4. On merge, the version is tagged and the charts are packaged and published to the [OpenFGA Helm repository](https://openfga.github.io/helm-charts) and to `oci://ghcr.io/openfga/helm-charts`.

> `patch`, `minor`, and `major` are conveniences: the workflow reads the current version, computes the next one, and hands it to release-please as an explicit release. Under the hood release-please only understands `auto` or an explicit version.

## When to use `explicit`

- **After a pre-release tag** (e.g. `0.5.0-beta.1`): release-please can't reliably guess the next version. If the last tag carried a pre-release suffix, use `explicit`.
- **After a manually created tag:** tags made outside the workflow are invisible to release-please's version logic, so anchor the next one with `explicit`.
- **For a beta:** release-please won't increment a pre-release suffix. Step it yourself — `0.5.0-beta.1` → `explicit: 0.5.0-beta.2` → `explicit: 0.5.0`.

## Changelog

Commits must follow [Conventional Commits](https://www.conventionalcommits.org/) so release-please can group them:

| Prefix                 | Changelog section |
| ---------------------- | ----------------- |
| `feat:`                | Added             |
| `fix:`                 | Fixed             |
| `perf:`, `refactor:`   | Changed           |
| `revert:`              | Removed           |
| `docs:`                | Documentation     |
| `test:`, `ci:`, `chore:` | _(hidden)_      |
