# Development

## Toolchain

`mise.toml` pins Go, `staticcheck`, `golangci-lint`, `just`, and `git-cliff` to
exact versions and also defines the tasks. The `justfile` only delegates to
them, so the pinned tools run rather than whatever is on `PATH`.

```bash
mise trust && mise install   # once per clone
just                         # list recipes
just check                   # full gate; run before asking for review
just format                  # gofmt -w
just install                 # build and copy onto PATH
just uninstall
```

`just install` copies the binary to `~/.local/bin`; override with
`INSTALL_DIR=/usr/local/bin just install`. Other recipes: `build`, `test`,
`lint`, `coverage`, `run`, `clean`. `just run scan` runs from source.

## The gate

`just check` runs, in order: `go vet` and `gofmt` verification (`lint` fails on
unformatted files), `go build`, `staticcheck`, `golangci-lint`, `go test`,
`go test -race`, and per-package coverage floors. It stops at the first
failure. The static-analysis steps run after the build because both need the
packages to type-check.

The coverage step (`scripts/coverage-floor.sh`) enforces a floor per package
from `coverage-floors.txt`. It fails when a package drops below its floor, when
a package has no floor, or when a floor names a package that no longer exists.
A package without tests needs an explicit floor of `0`. `just coverage` prints
the plain report.

Floors only go up. After a coverage pass, set the package's floor to the new
verified number. Coverage differs slightly between macOS and Linux, so use the
lower of the local and CI numbers.

Analysis work is split so no check runs twice:

| Step | Covers |
|---|---|
| `lint` | `go vet`, `gofmt` |
| `staticcheck` | the `SA`, `S`, `ST` and `U` checks |
| `golangci-lint` | `errcheck`, `ineffassign` (see `.golangci.yml`) |

`.golangci.yml` disables the linters another step already runs. Verify edits
with `golangci-lint config verify`.

## CI

`.github/workflows/ci.yml` runs `just check` on pushes to `main`, tags, and pull
requests, via `mise-action`. It clones full history because the build stamps the
version from `git describe --tags`. It runs with the runner's git config
neutralised (`GIT_CONFIG_NOSYSTEM`, `GIT_CONFIG_GLOBAL=/dev/null`) because the
runner's `safe.directory` setting would disable the ownership check some tests
rely on.

## Commits and releases

`main` only takes squash merges, and the squash commit's subject is the PR
title. `.github/workflows/pr-title.yml` validates that title, so it must be a
Conventional Commit subject, `type(scope): description`, with one of these
types:

| Commit type | Release |
|---|---|
| `feat` | minor |
| `fix`, `perf`, `refactor`, `revert` | patch |
| `chore`, `docs`, `test`, `style`, `ci`, `build` | none |

Major releases are disabled. A `!` marker (`feat!:`) or a
`BREAKING CHANGE:` / `BREAKING-CHANGE:` footer is rejected, not mapped to a
smaller bump, and the release workflow refuses any tag at `v1.0.0` or above.

The rules live in `internal/releasepolicy` and run through a small CLI:

```bash
echo "feat: add x" | go run ./scripts/release-policy validate-subject
git log v0.0.2..HEAD -z --format=%B | go run ./scripts/release-policy next-version --latest v0.0.2
go run ./scripts/release-policy check-tag v0.1.0
```

`next-version` reads full commit messages, NUL-terminated as `git log -z`
writes them, so footers in the body are checked too.

Exit codes: `0` clean, `1` a policy violation, `2` a usage error. `go run`
reports every non-zero exit as `1`; the split is visible from a built binary
(`go build -o release-policy ./scripts/release-policy`).

## Release

`.github/workflows/release.yml` runs on every push to `main`. After `just check`
passes it runs `next-version` over the commit messages since the latest tag
and tags the result. Merge commits and `chore(release):` commits are skipped by
subject; any other message that fails the policy fails the release rather than
being ignored. If a malformed message reaches `main`, the release job fails on
every later push until a valid `v0.Y.Z` tag is pushed on `HEAD`.

Pushing a `v*` tag releases that tag, and `workflow_dispatch` with a tag
back-fills one; both go through `check-tag`. A release builds archives for
linux and darwin on amd64 and arm64 with checksums, smoke-tests the
linux/amd64 binary, and publishes with `gh release create`. Windows is left out
because agentsync creates symlinks.

## Changelog

Generated from commit subjects by git-cliff using `cliff.toml`.

```bash
just changelog-preview   # unreleased changes
just changelog           # write CHANGELOG.md
```
