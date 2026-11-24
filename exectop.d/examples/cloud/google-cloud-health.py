#!/usr/bin/env python3
# exectop: short_name="Google Cloud public health" description="Check Google Cloud public service health incidents" references="https://status.cloud.google.com/" interval=5m timeout=15s tags=cloud,google,health requires=python3 network=required

from health_common import parse_google, run


raise SystemExit(run("Google Cloud", "https://status.cloud.google.com/incidents.json", parse_google))
