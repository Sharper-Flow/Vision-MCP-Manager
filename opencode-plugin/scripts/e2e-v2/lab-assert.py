#!/usr/bin/env python3
"""Assert qualification outcomes from lab fixture request evidence."""

import json
import sys

DAEMON = {
    "vision_list",
    "vision_remove",
    "vision_restart",
    "vision_init",
    "vision_status",
    "vision_guidance",
    "vision_slot_status",
    "vision_metrics",
}
CONTROL = {"opencode_mcp_connect", "opencode_mcp_disconnect"}

label, path, mode = sys.argv[1], sys.argv[2], sys.argv[3]
entries = [json.loads(line) for line in open(path, encoding="utf-8") if line.strip()]
bodies = [entry["body"] for entry in entries if isinstance(entry.get("body"), dict)]
with_tools = [body for body in bodies if body.get("tools")]
failures = []


def check(name, condition, detail=""):
    print(("OK: " if condition else "FAIL: ") + name + (" " + detail if detail else ""))
    if not condition:
        failures.append(name)


if not with_tools:
    check(label + " model request with tools", False, "no request carried a tools array")
else:
    names = {tool.get("function", {}).get("name") for tool in with_tools[0].get("tools", [])}
    if mode == "ten":
        expected = CONTROL | DAEMON
        check(label + " ten plugin tools", expected <= names, "missing=" + repr(sorted(expected - names)))
    elif mode == "two":
        check(
            label + " control tools only",
            CONTROL <= names and not names & DAEMON,
            repr(sorted(name for name in names if str(name).startswith("vision_") or str(name).startswith("opencode_mcp_"))),
        )
    elif mode == "server-direct":
        check(label + " per-server codemode off", "fixture_flip" in names, repr(sorted(name for name in names if str(name).startswith("fixture"))))
    elif mode == "control":
        results = []
        for message in (bodies[-1].get("messages", []) if bodies else []):
            if message.get("role") == "tool":
                results.append(message.get("content", ""))
        for index, (description, status) in enumerate((("disconnect", "disabled"), ("connect", "connected"))):
            try:
                value = json.loads(results[index])
            except (IndexError, ValueError, TypeError):
                value = {}
            check(label + " " + description, value.get("success") is True and value.get("status") == status, repr(value))
        try:
            missing = json.loads(results[2])
        except (IndexError, ValueError, TypeError):
            missing = {}
        check(label + " undeclared server", missing.get("success") is False and "not configured" in missing.get("error", ""), repr(missing))

raise SystemExit(1 if failures else 0)
