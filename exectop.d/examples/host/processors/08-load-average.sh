#!/bin/sh
set -eu
# exectop: short_name="load averages" description="Check the host 1, 5, and 15-minute load averages" interval=30s timeout=5s tags=host,load requires=awk requires_os=linux requires_path=/proc/loadavg

warn_1=${WARN_LOAD_1:-4.0}
maximum_1=${MAX_LOAD_1:-8.0}
warn_5=${WARN_LOAD_5:-4.0}
maximum_5=${MAX_LOAD_5:-8.0}
warn_15=${WARN_LOAD_15:-4.0}
maximum_15=${MAX_LOAD_15:-8.0}

case "$warn_1:$maximum_1:$warn_5:$maximum_5:$warn_15:$maximum_15" in
    ''|*[!0-9.:]*|*::*|:*|*:)
        echo "invalid load thresholds"
        echo "load thresholds must be non-negative decimal numbers" >&2
        exit 1
        ;;
esac

if [ ! -r /proc/loadavg ]; then
    echo "load average unavailable"
    echo "/proc/loadavg is not readable" >&2
    exit 1
fi

loads=$(awk '{ print $1, $2, $3 }' /proc/loadavg)
set -- $loads
load_1=${1:-}
load_5=${2:-}
load_15=${3:-}
if [ -z "$load_1" ] || [ -z "$load_5" ] || [ -z "$load_15" ] || ! awk \
    -v load_1="$load_1" -v load_5="$load_5" -v load_15="$load_15" \
    -v warn_1="$warn_1" -v maximum_1="$maximum_1" \
    -v warn_5="$warn_5" -v maximum_5="$maximum_5" \
    -v warn_15="$warn_15" -v maximum_15="$maximum_15" \
    'BEGIN { exit !(load_1 >= 0 && load_5 >= 0 && load_15 >= 0 && warn_1 >= 0 && maximum_1 >= warn_1 && warn_5 >= 0 && maximum_5 >= warn_5 && warn_15 >= 0 && maximum_15 >= warn_15) }'; then
    echo "invalid load average thresholds or value"
    echo "load averages and thresholds must be non-negative, with each MAX >= WARN" >&2
    exit 1
fi

summary="load average: 1m=$load_1 5m=$load_5 15m=$load_15"
emit_result() {
    printf '{"summary":"load average","value":%s,"threshold":%s,"unit":"load","load_5":%s,"load_15":%s}\n' "$load_1" "$maximum_1" "$load_5" "$load_15"
}
if awk \
    -v load_1="$load_1" -v load_5="$load_5" -v load_15="$load_15" \
    -v maximum_1="$maximum_1" -v maximum_5="$maximum_5" -v maximum_15="$maximum_15" \
    'BEGIN { exit !(load_1 >= maximum_1 || load_5 >= maximum_5 || load_15 >= maximum_15) }'; then
    emit_result; echo "$summary"
    echo "load average is at or above a maximum threshold" >&2
    exit 1
fi
if awk \
    -v load_1="$load_1" -v load_5="$load_5" -v load_15="$load_15" \
    -v warn_1="$warn_1" -v warn_5="$warn_5" -v warn_15="$warn_15" \
    'BEGIN { exit !(load_1 >= warn_1 || load_5 >= warn_5 || load_15 >= warn_15) }'; then
    emit_result; echo "$summary"
    echo "load average is at or above a warning threshold" >&2
    exit 2
fi

emit_result
echo "$summary"
exit 0
