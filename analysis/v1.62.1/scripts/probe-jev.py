#!/usr/bin/env python3
"""Opt-in single Jev request using a configured upstream key and synthetic data.

No CLI execution, retries, key rotation, or credential logging. Running with
--live may consume upstream quota. The saved result excludes request headers.
"""

import argparse
from datetime import datetime, timezone
import json
import os
from pathlib import Path
import re
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid


parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument("--live", action="store_true", required=True)
parser.add_argument("--output", type=Path, required=True)
args = parser.parse_args()
root = Path(__file__).resolve().parents[3]
env = dict(os.environ)
for line in (root / ".env").read_text().splitlines():
    line = line.strip()
    if not line or line.startswith("#") or "=" not in line:
        continue
    name, value = line.split("=", 1)
    if not env.get(name.strip()):
        env[name.strip()] = value.strip().strip("\"'")
keys = [key.strip() for key in env.get("COMMAND_CODE_API_KEY", "").split(",") if key.strip()]
if not keys:
    raise SystemExit("COMMAND_CODE_API_KEY is not configured")
base = (env.get("COMMAND_CODE_API_BASE") or "https://api.commandcode.ai").rstrip("/")
url = urllib.parse.urlparse(base)
if url.scheme != "https" or url.hostname != "api.commandcode.ai" or url.username or url.password:
    raise SystemExit("This probe expects the official HTTPS Command Code API host")

secrets = [*keys, env.get("PROXY_API_KEY", ""), env.get("FINGERPRINT_SEED", "")]


def redact(value):
    text = json.dumps(value, ensure_ascii=False)
    for secret in secrets:
        if secret:
            text = text.replace(secret, "[REDACTED]")
    text = re.sub(r"[\w.+-]+@[\w.-]+\.[A-Za-z]{2,}", "[EMAIL]", text)
    return json.loads(text)


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


body = {
    "model": "typesafe/jev",
    "state": {"value": 2},
    "questions": {
        "greater_than_one": {"type": "noul", "instructions": "Is the value greater than 1?"}
    },
}
route = "/provider/v1/systemone"
request = urllib.request.Request(
    base + route,
    data=json.dumps(body).encode(),
    headers={
        "Authorization": "Bearer " + keys[0],
        "Content-Type": "application/json",
        "User-Agent": "cli",
        "x-command-code-version": "1.62.1",
        "x-cli-environment": "production",
        "x-taste-learning": "false",
        "x-session-id": str(uuid.uuid4()),
        "x-project-slug": "command-code-protocol-probe",
    },
    method="POST",
)
report = {
    "checked_at": datetime.now(timezone.utc).isoformat(),
    "endpoint": base + route,
    "key_index": 1,
    "cli_version": "1.62.1",
    "request": body,
    "transport": "direct HTTPS; one synthetic request; no retry or key rotation",
}
start = time.monotonic()
try:
    try:
        response = urllib.request.build_opener(NoRedirect).open(request, timeout=55)
    except urllib.error.HTTPError as error:
        response = error
    with response:
        report["http_status"] = response.status
        report["content_type"] = response.headers.get("Content-Type")
        raw = response.read(1_048_577)
    report["body_truncated"] = len(raw) > 1_048_576
    try:
        data = json.loads(raw[:1_048_576])
        if isinstance(data, dict):
            report["top_level_fields"] = sorted(data)
            report["response"] = {
                key: redact(data[key]) for key in ("model", "answers", "usage", "success", "error")
                if key in data
            }
        else:
            report["response_type"] = type(data).__name__
    except (ValueError, UnicodeError):
        report["non_json_body"] = redact(raw[:800].decode(errors="replace"))
except Exception as error:
    report["transport_error_class"] = type(error).__name__
report["elapsed_seconds"] = round(time.monotonic() - start, 3)
text = json.dumps(report, indent=2, ensure_ascii=False) + "\n"
args.output.write_text(text)
print(text, end="")
