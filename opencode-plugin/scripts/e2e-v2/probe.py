#!/usr/bin/env python3
"""Structural isolation probe: live OpenCode paths must be invisible."""

import json
import os

PATHS = [
    "/home/jon/.opencode",
    "/home/jon/.config/opencode",
    "/home/jon/.local/share/opencode",
    "/home/jon/.local/share/opencode/opencode.db",
]


def hidden(path):
    return not os.path.exists(path) or (os.path.isdir(path) and not os.listdir(path))


result = {"live_paths_hidden": all(hidden(path) for path in PATHS), "exists": {path: os.path.exists(path) for path in PATHS}}
print(json.dumps(result))
raise SystemExit(0 if result["live_paths_hidden"] else 1)
