#!/bin/sh
set -eu
# exectop: short_name="Ethernet errors" description="Check that Ethernet interfaces have no observed RX/TX errors" interval=1m timeout=5s tags=host,network,ethernet requires=cat,basename requires_os=linux requires_path=/sys/class/net runbook="https://wiki.example/runbooks/ethernet-errors"

sysfs=/sys/class/net
if [ ! -d "$sysfs" ]; then
    echo "Ethernet interface statistics unavailable"
    echo "/sys/class/net is not readable" >&2
    exit 1
fi

interfaces=0
errors=0
summary=""
for iface in "$sysfs"/*; do
    [ -e "$iface" ] || continue
    [ -r "$iface/type" ] || continue
    type=$(cat "$iface/type")
    [ "$type" = "1" ] || continue
    interfaces=$((interfaces + 1))

    rx_file="$iface/statistics/rx_errors"
    tx_file="$iface/statistics/tx_errors"
    if [ ! -r "$rx_file" ] || [ ! -r "$tx_file" ]; then
        echo "Ethernet error statistics unavailable: $(basename "$iface")"
        echo "rx_errors or tx_errors is not readable for $(basename "$iface")" >&2
        exit 1
    fi
    rx=$(cat "$rx_file")
    tx=$(cat "$tx_file")
    case "$rx:$tx" in
        ''|*[!0-9:]*|*:*:*)
            echo "invalid Ethernet error statistics: $(basename "$iface")"
            echo "rx_errors and tx_errors must be non-negative integers" >&2
            exit 1
            ;;
    esac
    summary="$summary $(basename "$iface")(rx=$rx tx=$tx)"
    if [ "$rx" -ne 0 ] || [ "$tx" -ne 0 ]; then
        errors=$((errors + 1))
    fi
done

if [ "$interfaces" -eq 0 ]; then
    printf '{"summary":"Ethernet errors","value":0,"threshold":0,"unit":"interfaces"}\n'
    echo "no Ethernet interfaces found"
    exit 0
fi
if [ "$errors" -ne 0 ]; then
    printf '{"summary":"Ethernet errors","value":%s,"threshold":0,"unit":"interfaces"}\n' "$errors"
    echo "Ethernet interfaces with errors:$summary"
    echo "$errors of $interfaces Ethernet interfaces have observed RX/TX errors" >&2
    exit 1
fi

printf '{"summary":"Ethernet errors","value":0,"threshold":0,"unit":"interfaces"}\n'
echo "no Ethernet RX/TX errors observed:$summary"
exit 0
