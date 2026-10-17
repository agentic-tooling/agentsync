#!/bin/sh
# Fails when any package's coverage is below its floor in coverage-floors.txt,
# or when the floors and the packages go test reports disagree.
set -u

floors=${1:-coverage-floors.txt}
if [ ! -f "$floors" ]; then
    echo "coverage-floor: floors file $floors not found" >&2
    exit 2
fi
module=$(go list -m) || exit 2
out=$(mktemp) || exit 2
pkgs=$(mktemp) || { rm -f "$out"; exit 2; }
trap 'rm -f "$out" "$pkgs"' EXIT

# The authoritative package list, so a package missing from go test's output
# fails instead of passing unseen.
go list ./... >"$pkgs" || exit 2

# Captured to a file rather than piped so go test's exit status survives.
if ! go test -cover ./... >"$out" 2>&1; then
    cat "$out" >&2
    echo "coverage-floor: go test failed" >&2
    exit 1
fi

awk -v floorsfile="$floors" -v pkgsfile="$pkgs" -v prefix="$module/" '
function short(p) {
    return index(p, prefix) == 1 ? substr(p, length(prefix) + 1) : p
}
function fail(msg) {
    print "coverage-floor: " msg > "/dev/stderr"
    bad = 1
}
function check(p, actual) {
    seen[p] = 1
    parsed++
    if (!(p in floor)) {
        fail(p " " actual "% has no floor; add a line to " floorsfile)
    } else if (actual + 0 < floor[p] + 0) {
        fail(p " " actual "% is below its floor of " floor[p] "%")
    } else {
        print p " " actual " (floor " floor[p] ")"
    }
}
FILENAME == floorsfile {
    line = $0
    sub(/#.*/, "", line)
    n = split(line, f, " ")
    if (n == 0) next
    if (n != 2 || f[2] !~ /^[0-9]+(\.[0-9]+)?$/ || f[2] + 0 > 100) {
        fail(floorsfile ":" FNR ": malformed line, want <package> <percent>: " $0)
        next
    }
    k = short(f[1])
    if (k in floor) fail(floorsfile ":" FNR ": duplicate floor for " k)
    floor[k] = f[2]
    next
}
FILENAME == pkgsfile {
    if ($0 != "" && !(short($0) in seen)) fail("go test reported no coverage for " short($0))
    next
}
{
    # A no-test package with functions but no statements prints "\tpkg\t\t"
    # with no newline, so the next package line is joined onto it.
    while (match($0, /^\t[^\t]+\t\t/) && substr($0, RLENGTH + 1) !~ /^coverage:/) {
        check(short(substr($0, 2, RLENGTH - 3)), "0.0")
        $0 = substr($0, RLENGTH + 1)
    }
    if (split($0, f, "\t") < 3) next
    # A package with no tests counts as 0 so only an explicit floor of 0 admits it.
    if (match($0, /coverage: [0-9.]+%/)) {
        check(short(f[2]), substr($0, RSTART + 10, RLENGTH - 11))
    } else if ($0 ~ /\[no test files\]|\[no statements\]/) {
        check(short(f[2]), "0.0")
    }
}
END {
    if (parsed == 0) {
        fail("parsed no coverage lines from go test")
        exit 1
    }
    for (k in floor)
        if (!(k in seen)) fail(k " has a floor but go test reported no such package; remove the stale line")
    if (bad) exit 1
    print "==> coverage floors held"
}
' "$floors" "$out" "$pkgs"
