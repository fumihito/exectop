#!/bin/sh
set -eu
# exectop: short_name="SLAB usage" description="Check Linux kernel SLAB memory usage" interval=1m timeout=5s tags=host,memory,slab requires=awk requires_os=linux requires_path=/proc/meminfo runbook="https://wiki.example/runbooks/slab"

warn_mb=${WARN_SLAB_MB:-512}
maximum_mb=${MAX_SLAB_MB:-1024}

case "$warn_mb:$maximum_mb" in
    ''|*[!0-9:]*|*:*:*|*:|:*)
        echo "invalid SLAB thresholds"
        echo "WARN_SLAB_MB and MAX_SLAB_MB must be non-negative integers" >&2
        exit 1
        ;;
esac
if [ "$maximum_mb" -lt "$warn_mb" ]; then
    echo "invalid SLAB thresholds"
    echo "MAX_SLAB_MB must be greater than or equal to WARN_SLAB_MB" >&2
    exit 1
fi

if [ ! -r /proc/meminfo ]; then
    echo "SLAB information unavailable"
    echo "/proc/meminfo is not readable" >&2
    exit 1
fi

slab_kb=$(awk '
    /^Slab:/         { slab = $2 }
    /^SReclaimable:/ { reclaimable = $2 }
    /^SUnreclaim:/   { unreclaimable = $2 }
    END {
        if (slab == "" || reclaimable == "" || unreclaimable == "") exit 1
        print slab, reclaimable, unreclaimable
    }
' /proc/meminfo) || {
    echo "SLAB information unavailable"
    echo "Slab, SReclaimable, or SUnreclaim is missing from /proc/meminfo" >&2
    exit 1
}

set -- $slab_kb
slab_mb=$(( $1 / 1024 ))
reclaimable_mb=$(( $2 / 1024 ))
unreclaimable_mb=$(( $3 / 1024 ))
summary="SLAB: ${slab_mb}MiB (reclaimable: ${reclaimable_mb}MiB, unreclaimable: ${unreclaimable_mb}MiB; warn: ${warn_mb}MiB, maximum: ${maximum_mb}MiB)"
emit_result() {
    printf '{"summary":"SLAB usage","value":%s,"threshold":%s,"unit":"MiB","reclaimable_mb":%s,"unreclaimable_mb":%s}\n' "$slab_mb" "$maximum_mb" "$reclaimable_mb" "$unreclaimable_mb"
}

if [ "$slab_mb" -ge "$maximum_mb" ]; then
    emit_result; echo "$summary"
    echo "SLAB usage is at or above the maximum: ${slab_mb}MiB" >&2
    exit 1
fi
if [ "$slab_mb" -ge "$warn_mb" ]; then
    emit_result; echo "$summary"
    echo "SLAB usage is at or above the warning threshold: ${slab_mb}MiB" >&2
    exit 2
fi

emit_result
echo "$summary"
exit 0
