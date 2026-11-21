#!/usr/bin/env python3
"""Write the instance config and fixture script for one lab run."""

import json
import sys
from pathlib import Path

CONTROL_SCRIPT = [
    {"tool_calls": [{"id": "call-1", "type": "function", "function": {"name": "opencode_mcp_disconnect", "arguments": "{\"name\": \"fixture\"}"}}]},
    {"tool_calls": [{"id": "call-2", "type": "function", "function": {"name": "opencode_mcp_connect", "arguments": "{\"name\": \"fixture\"}"}}]},
    {"tool_calls": [{"id": "call-3", "type": "function", "function": {"name": "opencode_mcp_connect", "arguments": "{\"name\": \"not-declared\"}"}}]},
    {"content": "DONE"},
]
DONE_SCRIPT = [{"content": "DONE"}]


def main():
    instance, kind = Path(sys.argv[1]), sys.argv[2]
    config = {
        "update": "disable",
        "model": "stub/m",
        "providers": {
            "stub": {
                "name": "Stub",
                "package": "@opencode/ai/providers/openai-compatible",
                "settings": {"baseURL": "http://127.0.0.1:18765/v1", "apiKey": "fixture-not-a-secret"},
                "models": {"m": {"name": "M", "capabilities": {"tools": True, "input": ["text"], "output": ["text"]}, "limit": {"context": 32768, "output": 128}}},
            }
        },
        "plugins": [{"package": "/lab/workspace/install/node_modules/vision-plugin"}],
        "mcp": {"servers": {"fixture": {"type": "local", "command": ["/usr/bin/python3", "/lab/workspace/mcp-stdio.py"]}}},
        "permission": {"*": "allow"},
    }
    script = DONE_SCRIPT
    if kind == "broken":
        config["plugins"] = [{"package": "/lab/workspace/broken"}]
    elif kind == "on":
        config["experimental"] = {"full_codemode": True}
    elif kind == "server-direct":
        config["mcp"]["servers"]["fixture"]["codemode"] = False
    elif kind in ("control-off", "control-on"):
        script = CONTROL_SCRIPT
        if kind == "control-on":
            config["experimental"] = {"full_codemode": True}
    else:
        kind = "off"
    (instance / "home/.config/opencode/opencode.json").write_text(json.dumps(config, indent=2) + "\n")
    (instance / "workspace/fixture.json").write_text(json.dumps(script) + "\n")
    evidence = instance / "evidence"
    evidence.mkdir(exist_ok=True)
    (evidence / "fixture-position.json").write_text('{"position": 0}\n')
    (evidence / "fixture-requests.jsonl").unlink(missing_ok=True)


if __name__ == "__main__":
    main()
