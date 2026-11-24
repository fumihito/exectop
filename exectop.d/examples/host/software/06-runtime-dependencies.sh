#!/bin/sh
set -eu
# exectop: short_name="example dependencies" description="Check the runtime dependencies used by the bundled examples" interval=1h timeout=5s tags=system,examples

# The examples intentionally use portable userland tools instead of assuming a
# sqlite3 command. exectop embeds its SQLite driver, so sqlite3 is not a
# prerequisite; the binary only needs a writable state directory/database.
required_commands="sh awk df find mktemp"
missing=""
missing_count=0
for command_name in $required_commands; do
    if ! command -v "$command_name" >/dev/null 2>&1; then
        missing="$missing $command_name"
        missing_count=$((missing_count + 1))
    fi
done

if [ ! -r /proc/stat ] || [ ! -r /proc/meminfo ] || [ ! -r /proc/loadavg ]; then
    missing="$missing /proc"
    missing_count=$((missing_count + 1))
fi
if [ ! -d /sys/class/net ]; then
    missing="$missing /sys/class/net"
    missing_count=$((missing_count + 1))
fi

if [ "$missing_count" -ne 0 ]; then
    printf '{"summary":"example dependencies","value":%s,"threshold":0,"unit":"missing","missing":"%s"}\n' "$missing_count" "${missing# }"
    echo "missing example dependencies:${missing}"
    exit 1
fi

printf '{"summary":"example dependencies","value":0,"threshold":0,"unit":"missing","storage":"embedded-sqlite"}\n'
echo "all example dependencies available (SQLite is embedded in exectop)"
exit 0
