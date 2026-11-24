#!/usr/bin/env python3
# exectop: short_name="AWS public health" description="Check AWS public service health events" references="https://health.aws.amazon.com/health/status" interval=5m timeout=15s tags=cloud,aws,health requires=python3 network=required

import os

from health_common import parse_aws, run


def parse(data):
    # The UAE exclusion is enabled by default. Set AWS_HEALTH_IGNORE_UAE=0
    # (also false/no/off) when UAE events should be included.
    ignore_uae = os.environ.get("AWS_HEALTH_IGNORE_UAE", "1").lower() not in ("0", "false", "no", "off")
    return parse_aws(data, ignore_uae)


raise SystemExit(run("AWS", "https://status.aws.amazon.com/rss/all.rss", parse))
