#!/usr/bin/env python3
"""Stateless stdio MCP server for lab instances: one tool, JSON-RPC lines."""

import json
import sys

TOOLS = [
    {
        "name": "flip",
        "description": "fixture flip",
        "inputSchema": {"type": "object", "properties": {}},
    }
]


def respond(identifier, result):
    sys.stdout.write(json.dumps({"jsonrpc": "2.0", "id": identifier, "result": result}) + "\n")
    sys.stdout.flush()


for line in sys.stdin:
    line = line.strip()
    if not line:
        continue
    try:
        message = json.loads(line)
    except ValueError:
        continue
    if not isinstance(message, dict) or "id" not in message:
        continue
    method = message.get("method")
    if method == "initialize":
        version = message.get("params", {}).get("protocolVersion", "2025-03-26")
        respond(message["id"], {"protocolVersion": version, "capabilities": {"tools": {"listChanged": True}}, "serverInfo": {"name": "fixture", "version": "1"}})
    elif method == "tools/list":
        respond(message["id"], {"tools": TOOLS})
    elif method == "tools/call":
        name = message.get("params", {}).get("name")
        respond(message["id"], {"content": [{"type": "text", "text": "fixture:" + str(name)}]})
    else:
        respond(message["id"], {})
