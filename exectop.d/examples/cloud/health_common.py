#!/usr/bin/env python3
"""Small, dependency-free parsers for public cloud health feeds."""

import json
import re
import sys
from email.utils import parsedate_to_datetime
import urllib.error
import urllib.request
import xml.etree.ElementTree as ET


TIMEOUT = 10
ACTIVE = re.compile(r"\b(outage|outages|incident|operational issue|service interruption|service disruption|degraded|degradation|elevated error|unavailable|critical)\b", re.I)
TERMINAL = re.compile(r"\b(resolved|mitigated|recovered|closed|completed|restored|operating normally|no active)\b", re.I)
SEVERE = re.compile(r"\b(outage|outages|service interruption|unavailable|critical)\b", re.I)


def fetch(url):
    request = urllib.request.Request(url, headers={"User-Agent": "exectop-cloud-health/1"})
    with urllib.request.urlopen(request, timeout=TIMEOUT) as response:
        if response.status != 200:
            raise RuntimeError("HTTP status %s" % response.status)
        return response.read()


def xml_text(element):
    return " ".join("".join(element.itertext()).split())


def local_name(tag):
    return tag.rsplit("}", 1)[-1]


def rss_events(data):
    root = ET.fromstring(data)
    events = []
    for element in root.iter():
        if local_name(element.tag) in ("item", "entry"):
            events.append(" ".join(xml_text(child) for child in element))
    return events


def rss_items(data):
    root = ET.fromstring(data)
    items = []
    for element in root.iter():
        if local_name(element.tag) not in ("item", "entry"):
            continue
        fields = {local_name(child.tag): xml_text(child) for child in element}
        # AWS may publish a new GUID for every update to one health event.
        # The event link is the stable identity across those updates.
        identifier = fields.get("link") or fields.get("guid") or fields.get("id") or fields.get("title")
        items.append({"id": identifier, "text": " ".join(fields.values()), "date": fields.get("pubDate") or fields.get("published") or fields.get("updated")})
    return items


def rss_item_time(item, sequence):
    try:
        return (parsedate_to_datetime(item["date"]).timestamp(), sequence)
    except (TypeError, ValueError, OverflowError):
        return (0, -sequence)


def parse_rss(data):
    active = []
    for event in rss_events(data):
        # Public feeds may retain old entries. A terminal marker wins for an
        # entry, so historical resolved incidents do not cause an alert.
        if ACTIVE.search(event) and not TERMINAL.search(event):
            active.append(event)
    return active


def parse_aws(data, ignore_uae=True):
    events = rss_items(data)
    grouped = {}
    for sequence, event in enumerate(events):
        if not event["id"]:
            raise ValueError("AWS RSS item has no event ID")
        group = grouped.setdefault(event["id"], {"events": [], "text": []})
        group["events"].append((rss_item_time(event, sequence), event))
        group["text"].append(event["text"])

    region = re.compile(r"\b(me-central-1|UAE|United Arab Emirates)\b", re.I)
    latest = []
    for group in grouped.values():
        # Region names may occur only in an earlier update. Keep that context
        # when deciding whether the whole event should be excluded.
        if ignore_uae and region.search(" ".join(group["text"])):
            continue
        latest.append(max(group["events"], key=lambda item: item[0])[1])

    return [event["text"] for event in latest if ACTIVE.search(event["text"]) and not TERMINAL.search(event["text"])]


def parse_google(data):
    incidents = json.loads(data.decode("utf-8"))
    if not isinstance(incidents, list):
        raise ValueError("Google incident response is not a list")
    active = []
    for incident in incidents:
        if not isinstance(incident, dict):
            raise ValueError("Google incident entry is not an object")
        # The public JSON is a history file. Only an empty end means active.
        if incident.get("end"):
            continue
        impact = incident.get("status_impact", "")
        if impact in ("SERVICE_OUTAGE", "SERVICE_DISRUPTION"):
            active.append(incident)
    return active


def emit(provider, active, parser_error=None):
    if parser_error:
        print(json.dumps({"summary": provider + " public health: indeterminate", "value": 0, "threshold": 0, "unit": "incidents"}, separators=(",", ":")))
        print(parser_error, file=sys.stderr)
        return 3
    severe = sum(1 for event in active if SEVERE.search(json.dumps(event) if isinstance(event, dict) else event))
    state = "outage" if severe else ("degraded" if active else "healthy")
    print(json.dumps({"summary": provider + " public health: " + state, "value": len(active), "threshold": 0, "unit": "incidents"}, separators=(",", ":")))
    if active:
        print("%s active public health event(s)" % len(active), file=sys.stderr)
    print("%s public health: %s" % (provider, state))
    return 1 if severe else (2 if active else 0)


def run(provider, url, parser):
    try:
        return emit(provider, parser(fetch(url)))
    except (OSError, urllib.error.URLError, ET.ParseError, ValueError, RuntimeError) as error:
        return emit(provider, [], "%s: %s" % (provider + " health feed unavailable or changed", error))
