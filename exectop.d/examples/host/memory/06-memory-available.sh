#!/bin/sh
set -eu
# exectop: short_name="memory available" description="Check available host memory" interval=30s timeout=5s tags=host,memory requires=awk requires_os=linux requires_path=/proc/meminfo runbook="https://wiki.example/runbooks/memory"

warn_mb=${WARN_AVAILABLE_MB:-512}
minimum_mb=${MIN_AVAILABLE_MB:-256}

case "$warn_mb:$minimum_mb" in
    ''|*[!0-9:]*|*:*:*|*:|:*)
        echo "invalid memory thresholds: WARN_AVAILABLE_MB and MIN_AVAILABLE_MB must be integers" >&2
        echo "invalid memory thresholds"
        exit 1
        ;;
esac
if [ "$warn_mb" -lt "$minimum_mb" ]; then
    echo "invalid memory thresholds"
    echo "WARN_AVAILABLE_MB must be greater than or equal to MIN_AVAILABLE_MB" >&2
    exit 1
fi

if [ ! -r /proc/meminfo ]; then
    echo "memory information unavailable"
    echo "/proc/meminfo is not readable" >&2
    exit 1
fi

available_kb=$(awk '
    /^MemAvailable:/ { available = $2 }
    /^MemFree:/      { free = $2 }
    /^Buffers:/      { buffers = $2 }
    /^Cached:/       { cached = $2 }
    END {
        if (available != "") print available
        else if (free != "") print free + buffers + cached
    }
' /proc/meminfo)
if [ -z "$available_kb" ]; then
    echo "memory information unavailable"
    echo "MemAvailable could not be read from /proc/meminfo" >&2
    exit 1
fi

available_mb=$((available_kb / 1024))
summary="available memory: ${available_mb}MiB (warn: ${warn_mb}MiB, minimum: ${minimum_mb}MiB)"
emit_result() {
    printf '{"summary":"memory available","value":%s,"threshold":%s,"unit":"MiB"}\n' "$available_mb" "$minimum_mb"
}
if [ "$available_mb" -lt "$minimum_mb" ]; then
    emit_result; echo "$summary"
    echo "available memory is below the minimum: ${available_mb}MiB" >&2
    exit 1
fi
if [ "$available_mb" -lt "$warn_mb" ]; then
    emit_result; echo "$summary"
    echo "available memory is below the warning threshold: ${available_mb}MiB" >&2
    exit 2
fi

emit_result
echo "$summary"
exit 0
