#!/bin/sh
set -eu
# exectop: short_name="IO wait" description="Check the host CPU IO wait percentage" interval=30s timeout=10s tags=host,cpu,io requires=awk,sleep requires_os=linux requires_path=/proc/stat runbook="https://wiki.example/runbooks/iowait"

warn=${WARN_IOWAIT_PERCENT:-20.0}
maximum=${MAX_IOWAIT_PERCENT:-50.0}
# Five seconds smooths short /proc/stat accounting spikes, especially on VMs.
sample_seconds=${IOWAIT_SAMPLE_SECONDS:-5}

case "$warn:$maximum:$sample_seconds" in
    ''|*[!0-9.:]*|*::*|:*|*:)
        echo "invalid IO wait thresholds or sample interval"
        echo "WARN_IOWAIT_PERCENT, MAX_IOWAIT_PERCENT, and IOWAIT_SAMPLE_SECONDS must be non-negative numbers" >&2
        exit 1
        ;;
esac
if ! awk -v warn="$warn" -v maximum="$maximum" -v seconds="$sample_seconds" \
    'BEGIN { exit !(warn >= 0 && maximum >= warn && seconds > 0 && seconds <= 10) }'; then
    echo "invalid IO wait thresholds or sample interval"
    echo "MAX_IOWAIT_PERCENT must be >= WARN_IOWAIT_PERCENT and sample interval must be 0 < seconds <= 10" >&2
    exit 1
fi

read_cpu_sample() {
    awk '/^cpu / { print $5, $1 + $2 + $3 + $4 + $5 + $6 + $7 + $8; exit }' /proc/stat
}

if [ ! -r /proc/stat ]; then
    echo "IO wait unavailable"
    echo "/proc/stat is not readable" >&2
    exit 1
fi

before=$(read_cpu_sample)
set -- $before
before_iowait=${1:-}
before_total=${2:-}
if [ -z "$before_iowait" ] || [ -z "$before_total" ]; then
    echo "IO wait unavailable"
    echo "CPU counters are missing from /proc/stat" >&2
    exit 1
fi

sleep "$sample_seconds"
after=$(read_cpu_sample)
set -- $after
after_iowait=${1:-}
after_total=${2:-}
if [ -z "$after_iowait" ] || [ -z "$after_total" ]; then
    echo "IO wait unavailable"
    echo "CPU counters could not be read twice from /proc/stat" >&2
    exit 1
fi

percent=$(awk -v bi="$before_iowait" -v bt="$before_total" -v ai="$after_iowait" -v at="$after_total" \
    'BEGIN { di = ai - bi; dt = at - bt; if (di < 0 || dt <= 0) exit 1; printf "%.1f", 100 * di / dt }') || {
    echo "IO wait sample invalid"
    echo "CPU counters did not advance monotonically" >&2
    exit 1
}
summary="IO wait: ${percent}% (warn: ${warn}%, maximum: ${maximum}%)"
emit_result() {
    printf '{"summary":"IO wait","value":%s,"threshold":%s,"unit":"percent"}\n' "$percent" "$maximum"
}
if awk -v value="$percent" -v maximum="$maximum" 'BEGIN { exit !(value >= maximum) }'; then
    emit_result; echo "$summary"
    echo "IO wait is at or above the maximum threshold" >&2
    exit 1
fi
if awk -v value="$percent" -v warn="$warn" 'BEGIN { exit !(value >= warn) }'; then
    emit_result; echo "$summary"
    echo "IO wait is at or above the warning threshold" >&2
    exit 2
fi

emit_result
echo "$summary"
exit 0
