#!/bin/sh
set -eu
# exectop: short_name="internet reachable" description="Check whether a URL can be reached" interval=30s timeout=10s tags=network network=required

url=${URL:-https://www.example.com/}
timeout=${TIMEOUT_SECONDS:-5}

case "$timeout" in
    ''|*[!0-9]*) echo "invalid timeout: $timeout"; echo "TIMEOUT_SECONDS must be a positive integer" >&2; exit 1 ;;
esac
if [ "$timeout" -le 0 ]; then
    echo "invalid timeout: $timeout"
    echo "TIMEOUT_SECONDS must be a positive integer" >&2
    exit 1
fi

if command -v curl >/dev/null 2>&1; then
    if curl --fail --silent --show-error --location --max-time "$timeout" "$url" >/dev/null; then
		printf '{"summary":"internet reachable","target":"%s","value":1,"threshold":1,"unit":"boolean"}\n' "$url"
        echo "reachable: $url"
        exit 0
    fi
elif command -v wget >/dev/null 2>&1; then
    if wget --quiet --timeout="$timeout" --spider "$url"; then
		printf '{"summary":"internet reachable","target":"%s","value":1,"threshold":1,"unit":"boolean"}\n' "$url"
        echo "reachable: $url"
        exit 0
    fi
else
    echo "unavailable: curl or wget"
    echo "curl or wget is required" >&2
    exit 1
fi

printf '{"summary":"internet reachable","target":"%s","value":0,"threshold":1,"unit":"boolean"}\n' "$url"
echo "unreachable: $url"
echo "request failed: $url" >&2
exit 1
