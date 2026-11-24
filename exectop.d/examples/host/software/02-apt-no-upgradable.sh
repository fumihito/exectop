#!/bin/sh
set -eu
# exectop: short_name="apt updates" description="Check for upgradable apt packages" interval=1h timeout=30s tags=packages,apt requires=apt-get,awk

if ! command -v apt-get >/dev/null 2>&1; then
    echo "apt-get unavailable"
    echo "apt-get was not found" >&2
    exit 1
fi

if ! result=$(mktemp); then
    echo "unable to create temporary file"
    echo "mktemp failed" >&2
    exit 1
fi
trap 'rm -f "$result"' EXIT HUP INT TERM

if ! LC_ALL=C DEBIAN_FRONTEND=noninteractive apt-get -s -q upgrade >"$result" 2>&1; then
    echo "apt upgrade simulation failed"
    cat "$result" >&2
    exit 1
fi

# Count only packages that the simulated upgrade would install. Packages
# listed as deferred due to phasing do not produce an Inst line.
upgradable=$(awk '$1 == "Inst" { count++ } END { print count + 0 }' "$result")
if [ "$upgradable" -eq 0 ]; then
    printf '{"summary":"apt updates","value":%s,"threshold":0,"unit":"packages"}\n' "$upgradable"
    echo "no upgradable packages"
    exit 0
fi

printf '{"summary":"apt updates","value":%s,"threshold":0,"unit":"packages"}\n' "$upgradable"
echo "upgradable packages: $upgradable"
echo "Upgradable packages: $upgradable" >&2
exit 1
