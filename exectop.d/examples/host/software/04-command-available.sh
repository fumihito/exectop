#!/bin/sh
set -eu
# exectop: short_name="command available" description="Check whether a command is installed" interval=1h timeout=5s tags=system

command_name=${1:-go}
if command -v "$command_name" >/dev/null 2>&1; then
	printf '{"summary":"command available","target":"%s","value":1,"unit":"boolean"}\n' "$command_name"
    echo "command available: $command_name"
    exit 0
fi

printf '{"summary":"command unavailable","target":"%s","value":0,"unit":"boolean"}\n' "$command_name"
echo "command unavailable: $command_name"
echo "Command not found: $command_name" >&2
exit 1
