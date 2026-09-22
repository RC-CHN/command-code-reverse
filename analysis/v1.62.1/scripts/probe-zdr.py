#!/usr/bin/env python3
"""Opt-in ZDR routing comparison: at most three short synthetic requests.

Uses the first configured upstream key. No retries, key rotation, CLI execution,
or credential logging. One explicit non-ZDR control request is included; this
is a test case, not a production fallback. May consume upstream quota.
"""

import argparse
from datetime import datetime, timezone
import json
import os
from pathlib import Path
import re
import time
import urllib.error
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
if base != "https://api.commandcode.ai":
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
    "config": {
        "workingDir": "/tmp/zdr-probe",
        "date": datetime.now(timezone.utc).date().isoformat(),
        "environment": "linux",
        "structure": [],
        "isGitRepo": False,
        "currentBranch": "",
        "mainBranch": "",
        "gitStatus": "",
        "recentCommits": [],
    },
    "memory": None,
    "taste": None,
    "skills": None,
    "permissionMode": "standard",
    "mode": "agent",
    "threadId": str(uuid.uuid4()),
    "params": {
        "model": "",
        "messages": [{"role": "user", "content": [{"type": "text", "text": "Reply OK."}]}],
        "tools": [],
        "system": "Reply with exactly OK. No explanation.",
        "max_tokens": 96,
        "reasoning_effort": "low",
        "stream": True,
    },
}
report = {
    "checked_at": datetime.now(timezone.utc).isoformat(),
    "endpoint": base + "/alpha/generate",
    "key_index": 1,
    "cli_version": "1.62.1",
    "transport": "direct HTTPS; synthetic prompts; no retries, redirects, or key rotation",
    "scope": "Tests observable routing policy, not actual server-side data deletion or retention.",
    "request_template": body,
    "cases": [],
}
opener = urllib.request.build_opener(NoRedirect)
for name, model, enabled in [
    ("unsupported_zdr_on", "Qwen/Qwen3.8-Max-0902", True),
    ("unsupported_zdr_off_control", "Qwen/Qwen3.8-Max-0902", False),
    ("supported_zdr_on", "deepseek/deepseek-v4-flash", True),
]:
    headers = {
        "Authorization": "Bearer " + keys[0],
        "Content-Type": "application/json",
        "User-Agent": "cli",
        "x-command-code-version": "1.62.1",
        "x-cli-environment": "production",
        "x-taste-learning": "false",
        "x-session-id": body["threadId"],
        "x-project-slug": "tmp-zdr-probe",
        "traceparent": "00-" + uuid.uuid4().hex + "-" + uuid.uuid4().hex[:16] + "-01",
    }
    if enabled:
        headers["x-cmd-zdr"] = "1"
    request_body = {**body, "params": {**body["params"], "model": model}}
    request = urllib.request.Request(
        report["endpoint"], data=json.dumps(request_body).encode(), headers=headers, method="POST"
    )
    result = {"name": name, "model": model, "zdr_header": "1" if enabled else None}
    start = time.monotonic()
    try:
        try:
            response = opener.open(request, timeout=45)
        except urllib.error.HTTPError as error:
            response = error
        with response:
            result["http_status"] = response.status
            result["content_type"] = response.headers.get("Content-Type")
            raw = response.read(1_048_577)
        result["body_truncated"] = len(raw) > 1_048_576
        events = []
        for line in raw[:1_048_576].splitlines():
            try:
                event = json.loads(line)
                if isinstance(event, dict):
                    events.append(event)
            except (ValueError, UnicodeError):
                pass
        result["event_types"] = sorted({event["type"] for event in events if "type" in event})
        result["errors"] = [event.get("error", event.get("message")) for event in events
                            if "error" in event or event.get("type") == "error"]
        result["text"] = "".join(event.get("text", "") for event in events
                                if event.get("type") == "text-delta")[:200]
        result["usage"] = next((event.get("totalUsage", event.get("usage")) for event in reversed(events)
                                if event.get("totalUsage") or event.get("usage")), None)
        result["cost_usd"] = next((event["providerMetadata"]["gateway"]["cost"] for event in reversed(events)
                                   if "cost" in event.get("providerMetadata", {}).get("gateway", {})), None)
        if not events:
            result["unparsed_body"] = raw[:800].decode(errors="replace")
    except Exception as error:
        result["transport_error_class"] = type(error).__name__
    result["elapsed_seconds"] = round(time.monotonic() - start, 3)
    result = redact(result)
    report["cases"].append(result)
    args.output.write_text(json.dumps(redact(report), indent=2, ensure_ascii=False) + "\n")
    print(json.dumps(result, ensure_ascii=False), flush=True)
    # Do not multiply an outage, invalid credential, or exhausted quota.
    status = result.get("http_status", 0)
    if "transport_error_class" in result or status in (401, 402, 429) or status >= 500:
        break
