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
`go test -race`, and per-package coverage. It stops at the first failure. The
static-analysis steps run after the build because both need the packages to
type-check.

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

## Release

`.github/workflows/release.yml` runs on every push to `main`. After `just check`
passes it reads Conventional Commit subjects since the latest tag and tags the
next version:

| Commit type | Bump |
|---|---|
| `feat` | minor |
| `fix`, `perf`, `refactor`, `chore` | patch |
| anything else | no release |

A breaking-change marker bumps minor, not major; majors are cut by hand by
pushing a `v*` tag. Pushing a `v*` tag releases that tag, and
`workflow_dispatch` with a tag back-fills one. A release builds archives for
linux and darwin on amd64 and arm64 with checksums, smoke-tests the
linux/amd64 binary, and publishes with `gh release create`. Windows is left out
because agentsync creates symlinks.

Choose the commit type deliberately: it decides whether a push releases.

## Changelog

Generated from commit subjects by git-cliff using `cliff.toml`.

```bash
just changelog-preview   # unreleased changes
just changelog           # write CHANGELOG.md
```
