# Front door only: every recipe delegates to a mise task, so the toolchain
# pinned in mise.toml is what runs, not whatever happens to be on PATH.

set positional-arguments

default:
    @just --list

# Full pre-review gate: lint, build, static analysis, test, race, coverage
check:
    mise run check

build:
    mise run build

test:
    mise run test

lint:
    mise run lint

format:
    mise run format

coverage:
    mise run coverage

# Run from source: just run scan --json
run *args:
    mise run run -- "$@"

# Build and copy onto PATH (INSTALL_DIR, default ~/.local/bin)
install:
    mise run install

uninstall:
    mise run uninstall

clean:
    mise run clean

# Regenerate CHANGELOG.md from conventional commits
changelog:
    git cliff -o CHANGELOG.md

# Preview unreleased changes
changelog-preview:
    git cliff --unreleased
