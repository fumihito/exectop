#!/bin/sh
set -eu
# exectop: short_name="disk free" description="Check free space on a filesystem" interval=1m timeout=5s tags=disk,storage requires=df,awk runbook="https://wiki.example/runbooks/disk-free"

path=${CHECK_PATH:-/}
minimum=${MIN_FREE_PERCENT:-10}

case "$minimum" in
    ''|*[!0-9]*) echo "invalid minimum free space: $minimum"; echo "MIN_FREE_PERCENT must be an integer" >&2; exit 1 ;;
esac
if [ "$minimum" -gt 100 ]; then
    echo "invalid minimum free space: $minimum"
    echo "MIN_FREE_PERCENT must be between 0 and 100" >&2
    exit 1
fi

available=$(LC_ALL=C df -P "$path" | awk 'NR == 2 { gsub(/%/, "", $5); print 100 - $5 }')
if [ -z "$available" ]; then
    echo "free space unavailable: $path"
    echo "Unable to determine free space: $path" >&2
    exit 1
fi

emit_result() {
    printf '{"summary":"disk free","value":%s,"threshold":%s,"unit":"percent"}\n' "$available" "$minimum"
}

if [ "$available" -ge "$minimum" ]; then
    emit_result
    echo "free space: ${available}% (minimum: ${minimum}%)"
    exit 0
fi

emit_result
echo "free space: ${available}% (minimum: ${minimum}%)"
echo "Insufficient free space: ${available}% (minimum: ${minimum}%)" >&2
exit 1
