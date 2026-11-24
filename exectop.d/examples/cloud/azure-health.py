#!/usr/bin/env python3
# exectop: short_name="Azure public health" description="Check Azure public service health events" references="https://azure.status.microsoft/en-us/status/" interval=5m timeout=15s tags=cloud,azure,health requires=python3 network=required

from health_common import parse_rss, run


raise SystemExit(run("Azure", "https://azure.status.microsoft/en-us/status/feed/", parse_rss))
