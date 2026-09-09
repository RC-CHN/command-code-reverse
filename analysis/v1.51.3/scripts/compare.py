#!/usr/bin/env python3
"""Compare beautified CLI bundles without importing or executing either CLI.

Usage: python3 compare.py OLD_CLI NEW_CLI
The scanner expects Prettier's default two-space indentation. Function
hash changes are review candidates, not proof of a semantic protocol change:
minifier identifier renaming also changes these hashes.
"""

import hashlib
import json
import re
import sys
from pathlib import Path


FUNCTIONS = [
    "buildCommandAuthHeaders", "buildCommandApiHeaders", "buildMachineFingerprint",
    "createModelClient", "createSystemPromptBuilder", "toWireSystem",
    "toWireMessages", "toWireTools", "toWirePermissionMode", "toWireThreadId",
    "readStreamErrorEvent", "consumeStream", "readCacheWriteTokens1h",
    "parseSpendCapError",
]


def digest(text):
    return hashlib.sha256(text.encode()).hexdigest()


def functions(source):
    found = {}
    for name in FUNCTIONS:
        match = re.search(
            r"^(?:async )?function " + re.escape(name) + r"\([\s\S]*?^}",
            source, re.MULTILINE,
        )
        if match:
            found[name] = {
                "line": source.count("\n", 0, match.start()) + 1,
                "sha256": digest(match[0]),
            }
    return found


def models(source):
    found = {}
    for match in re.finditer(
        r"^    ([A-Z][A-Z0-9_]+): \{\n(.*?)^    \}",
        source, re.MULTILINE | re.DOTALL,
    ):
        body = match[2]
        mid = re.search(r'^      id: "([^"]+)"', body, re.MULTILINE)
        name = re.search(r'^      name: "([^"]+)"', body, re.MULTILINE)
        context = re.search(r"^      contextWindow: ([\de.+]+)", body, re.MULTILINE)
        if mid and name and context:
            found[mid[1]] = {
                "id": mid[1], "name": name[1],
                "context_length": int(float(context[1])),
            }
    if not found:
        raise ValueError("No catalog entries found; check formatter/layout")
    return found


def routes(source):
    # Literal endpoints only: template-built paths require manual review.
    return set(re.findall(r'"(/(?:alpha|provider)/[^"\n]+)"', source))


def compare(old_path, new_path):
    old, new = old_path.read_text(), new_path.read_text()
    old_models, new_models = models(old), models(new)
    old_funcs, new_funcs = functions(old), functions(new)
    return {
        "old": {"path": str(old_path), "sha256": digest(old)},
        "new": {"path": str(new_path), "sha256": digest(new)},
        "functions": {
            name: {"old": old_funcs.get(name), "new": new_funcs.get(name)}
            for name in FUNCTIONS
        },
        "catalog_counts": {"old": len(old_models), "new": len(new_models)},
        "added_models": [new_models[i] for i in sorted(new_models.keys() - old_models.keys())],
        "removed_models": sorted(old_models.keys() - new_models.keys()),
        "changed_models": [
            {"old": old_models[i], "new": new_models[i]}
            for i in sorted(old_models.keys() & new_models.keys())
            if old_models[i] != new_models[i]
        ],
        "added_literal_routes": sorted(routes(new) - routes(old)),
        "removed_literal_routes": sorted(routes(old) - routes(new)),
    }


if __name__ == "__main__":
    if len(sys.argv) != 3:
        raise SystemExit(__doc__)
    print(json.dumps(compare(Path(sys.argv[1]), Path(sys.argv[2])), indent=2))
