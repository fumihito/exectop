#!/bin/sh
set -eu

# Run from the project root when invoked by exectop. This also makes the
# check harmless on hosts where the source tree is not deployed.
# exectop: short_name="exectop freshness" description="Check whether exectop is newer than its source" cwd=../../../.. interval=30s timeout=5s tags=build,freshness requires=find,awk

source_count=$(find . -type f \( -name '*.go' -o -name 'go.mod' -o -name 'go.sum' \) -print | awk 'END { print NR + 0 }')
if [ "$source_count" -eq 0 ]; then
    printf '{"summary":"source check","value":0,"threshold":0,"unit":"files"}\n'
    echo "no Go source files found"
    exit 0
fi

binary=${EXECTOP_BINARY:-}
if [ -z "$binary" ] && [ -x ./exectop ]; then
	binary=./exectop
fi
if [ -z "$binary" ]; then
	binary=$(command -v exectop 2>/dev/null || true)
fi
if [ -z "$binary" ] || [ ! -f "$binary" ] || [ ! -x "$binary" ]; then
    echo "exectop binary unavailable"
    echo "exectop binary was not found" >&2
    exit 1
fi

find . -type f \( -name '*.go' -o -name 'go.mod' -o -name 'go.sum' \) -print |
while IFS= read -r source; do
    if [ "$binary" -ot "$source" ]; then
        echo "stale binary: $source"
        echo "exectop binary is older than source: $source" >&2
        exit 1
    fi
done

printf '{"summary":"exectop freshness","value":1,"threshold":1,"unit":"boolean","source_files":%s}\n' "$source_count"
echo "exectop binary is fresh ($source_count source files)"
exit 0
