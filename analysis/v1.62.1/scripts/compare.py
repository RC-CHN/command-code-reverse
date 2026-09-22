#!/usr/bin/env python3
"""Statically compare 1.54.2 and 1.62.1; never import or execute either CLI.

Run from the repository root. Requires the two Prettier 3.6.2 formatted
bundles in references/. The explicit binding map was manually reviewed
for these two releases; this is not a general JavaScript equivalence test.
"""

import json
import re
import runpy
from pathlib import Path


base = runpy.run_path("analysis/v1.51.3/scripts/compare.py")
old_path = Path("references/command-code-npm/extracted-1.54.2/package/dist/cli.beautified.mjs")
new_path = Path("references/command-code-npm/extracted-1.62.1/package/dist/cli.beautified.mjs")
old, new = old_path.read_text(), new_path.read_text()
result = base["compare"](old_path, new_path)
digest = base["digest"]

# These renamed bindings all have identical initializer text in both bundles.
renames = {
    "Sy": "Gy", "wy": "Vy", "sb": "Ub", "ob": "Fb", "ok": "Dk",
    "sk": "$k", "Ph": "Kh", "nk": "Ok", "rk": "Lk", "yv": "Wv",
    "ky": "Ky", "ik": "Nk", "rw": "Iw", "nw": "xw",
}


def initializer(source, name):
    match = re.search(
        r"^      \(" + re.escape(name) + r" =\s*([\s\S]*?)\),$",
        source, re.MULTILINE,
    )
    if not match:
        raise ValueError("Missing initializer: " + name)
    return match[1].strip(), source.count("\n", 0, match.start()) + 1


bindings = []
for before, after in renames.items():
    a, old_line = initializer(old, before)
    b, new_line = initializer(new, after)
    if a != b:
        raise ValueError("Changed initializer: " + before + " -> " + after)
    bindings.append({
        "old": before, "new": after, "old_line": old_line,
        "new_line": new_line, "initializer_sha256": digest(a),
    })
result["verified_renamed_bindings"] = bindings
rename_pattern = re.compile(
    r"(?<![\w$])(?:" + "|".join(map(re.escape, renames)) + r")(?![\w$])"
)


def function(source, name):
    match = re.search(
        r"^(?:async )?function(?:\*)? " + re.escape(name) + r"\([\s\S]*?^}",
        source, re.MULTILINE,
    )
    if not match:
        raise ValueError("Missing function: " + name)
    return match[0], source.count("\n", 0, match.start()) + 1


helpers = [
    "createNodeTransport", "createCommandApiClient", "readLines",
    "guardStreamFailures", "isStreamErrorRetryable", "hasTerminalMarker",
    "toWireToolOutput", "toWireToolName", "coerceToolInput",
    "formatServerToolOutput", "getApiBaseUrl",
]
reviews = {}
for name in [*result["functions"], *helpers]:
    a, old_line = function(old, name)
    b, new_line = function(new, name)
    normalized = rename_pattern.sub(lambda m: renames[m[0]], a)
    if a == b:
        classification = "identical"
    elif normalized == b:
        classification = "verified_binding_renames_only"
    elif name == "createModelClient" and normalized == b.replace(
        "startChatSpan({\n          model: t.model,\n"
        "          toolCount: t.tools.length,\n"
        "          conversationId: e.conversationId,\n        })",
        "startChatSpan({ model: t.model, toolCount: t.tools.length })",
    ):
        classification = "adds_conversation_id_to_telemetry_only"
    else:
        raise ValueError("Unreviewed function change: " + name)
    reviews[name] = {
        "classification": classification, "old_line": old_line,
        "new_line": new_line, "old_sha256": digest(a), "new_sha256": digest(b),
    }
result["reviewed_functions"] = reviews


def catalog_metadata(source):
    entries = {}
    for match in re.finditer(
        r"^    ([A-Z][A-Z0-9_]+): \{\n(.*?)^    \}",
        source, re.MULTILINE | re.DOTALL,
    ):
        body = match[2]
        mid = re.search(r'^      id: "([^"]+)"', body, re.MULTILINE)
        if not mid or not re.search(r'^      name: "', body, re.MULTILINE):
            continue
        item = {}
        for field in ("hidden", "reasoning", "inputModalities", "reasoningEfforts", "maxOutputTokens"):
            value = re.search(r"^      " + field + r": (.+),$", body, re.MULTILINE)
            if value:
                item[field] = json.loads(value[1].replace("!0", "true").replace("!1", "false"))
        entries[mid[1]] = item
    return entries


old_meta, new_meta = catalog_metadata(old), catalog_metadata(new)
result["catalog_counts_scope"] = "entries with an explicit contextWindow (legacy comparator)"
result["full_catalog_counts"] = {"old": len(old_meta), "new": len(new_meta)}
result["hidden_catalog_counts"] = {
    "old": sum(bool(entry.get("hidden")) for entry in old_meta.values()),
    "new": sum(bool(entry.get("hidden")) for entry in new_meta.values()),
}
result["added_model_metadata"] = {
    key: new_meta[key] for key in sorted(new_meta.keys() - old_meta.keys())
}
result["changed_model_metadata"] = {
    key: {"old": old_meta[key], "new": new_meta[key]}
    for key in sorted(old_meta.keys() & new_meta.keys())
    if old_meta[key] != new_meta[key]
}
print(json.dumps(result, indent=2, ensure_ascii=False))
