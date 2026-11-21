#!/usr/bin/env python3
"""Structural isolation probe: live OpenCode paths under REAL_HOME must be invisible."""

import json
import os

home = os.environ["REAL_HOME"]
paths = [os.path.join(home, path) for path in (".opencode", ".config/opencode", ".local/share/opencode", ".local/share/opencode/opencode.db")]


def hidden(path):
    return not os.path.exists(path) or (os.path.isdir(path) and not os.listdir(path))


result = {"live_paths_hidden": all(hidden(path) for path in paths), "exists": {path: os.path.exists(path) for path in paths}}
print(json.dumps(result))
raise SystemExit(0 if result["live_paths_hidden"] else 1)
