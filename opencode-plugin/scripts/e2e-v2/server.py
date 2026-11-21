#!/usr/bin/env python3
"""Loopback-only provider and MCP fixture for the real-host qualification."""

import json
import os
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

root = os.environ["FIXTURE"]
phase = {}
state = {"generation": 0, "session": 0, "reconnect_probe": False}


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, fmt, *args):
        with open(root + "/server.log", "a", encoding="utf-8") as out:
            out.write((fmt % args) + "\n")

    def reply(self, status, body, headers=None):
        data = body if isinstance(body, bytes) else json.dumps(body).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        for key, value in (headers or {}).items():
            self.send_header(key, value)
        self.end_headers()
        self.wfile.write(data)

    def do_GET(self):
        if self.path == "/health":
            return self.reply(200, {"ok": True})
        if self.path == "/reset":
            phase.clear()
            state["generation"] = 0
            state["reconnect_probe"] = False
            return self.reply(200, {"ok": True})
        if self.path == "/reconnect-probe":
            state["reconnect_probe"] = True
            return self.reply(200, {"ok": True})
        self.reply(405, {"error": "no event stream"})

    def do_DELETE(self):
        if self.path == "/mcp" and state["reconnect_probe"]:
            state["generation"] = 1
        self.reply(200, {})

    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        if self.path == "/mcp":
            return self.mcp(body)
        with open(root + "/requests.jsonl", "a", encoding="utf-8") as out:
            out.write(json.dumps(body) + "\n")
        messages = body.get("messages", [])
        user = next((str(m.get("content", "")) for m in reversed(messages) if m.get("role") == "user" and not str(m.get("content", "")).startswith("<system-update>")), "")
        key = next((s for s in ("control", "expiry", "reconnect") if s in user), "matrix")
        calls = []
        if body.get("tools"):
            n = phase.get(key, 0)
            phase[key] = n + 1
            if key == "control":
                calls = [
                    ("opencode_mcp_disconnect", {"name": "fixture"}),
                    ("opencode_mcp_connect", {"name": "fixture"}),
                    ("opencode_mcp_connect", {"name": "not-declared"}),
                ][n : n + 1]
            elif key == "expiry":
                calls = [
                    ("execute", {"code": "return await tools.fixture.flip({action: 'invalid'})"}),
                    ("execute", {"code": "return await tools.fixture.flip({})"}),
                    ("execute", {"code": "return await tools.fixture.flip({})"}),
                    ("execute", {"code": "return await tools.$codemode.search({query: 'fixture', namespace: 'fixture'})"}),
                    ("execute", {"code": "return await search({query: 'new_tool'})"}),
                    ("execute", {"code": "return await tools.fixture.new_tool({value: 'hello'})"}),
                    ("execute", {"code": "return await tools.fixture.new_tool({value: 'invalid'})"}),
                ][n : n + 1]
            elif key == "reconnect":
                calls = [
                    ("opencode_mcp_disconnect", {"name": "fixture"}),
                    ("opencode_mcp_connect", {"name": "fixture"}),
                    ("execute", {"code": "const catalog = await search({query: 'new_tool'}); const result = await tools.fixture.new_tool({value: 'hello'}); return {catalog, result}"}),
                ][n : n + 1]
        tool_calls = [
            {"index": i, "id": "call_%s_%s" % (key, phase.get(key, 0)), "type": "function", "function": {"name": name, "arguments": json.dumps(args)}}
            for i, (name, args) in enumerate(calls)
        ]
        model = body.get("model", "model")
        base = {"id": "stub", "object": "chat.completion.chunk", "created": int(time.time()), "model": model}
        delta = {"role": "assistant", "tool_calls": tool_calls} if calls else {"role": "assistant", "content": "DONE"}
        chunks = [dict(base, choices=[{"index": 0, "delta": delta}]), dict(base, choices=[{"index": 0, "delta": {}, "finish_reason": "tool_calls" if calls else "stop"}])]
        if body.get("stream"):
            data = b"".join(("data: " + json.dumps(chunk) + "\n\n").encode() for chunk in chunks) + b"data: [DONE]\n\n"
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream")
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)
        else:
            self.reply(200, {"id": "stub", "object": "chat.completion", "created": int(time.time()), "model": model, "choices": [{"index": 0, "message": delta, "finish_reason": "tool_calls" if calls else "stop"}], "usage": {"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2}})

    def mcp(self, body):
        with open(root + "/mcp.jsonl", "a", encoding="utf-8") as out:
            out.write(json.dumps({"headers": dict(self.headers), "body": body}) + "\n")
        method = body.get("method")
        sid = self.headers.get("Mcp-Session-Id")
        if method == "initialize":
            state["session"] += 1
            result = {"protocolVersion": "2025-03-26", "capabilities": {"tools": {"listChanged": True}}, "serverInfo": {"name": "fixture", "version": "1"}}
            return self.reply(200, {"jsonrpc": "2.0", "id": body["id"], "result": result}, {"Mcp-Session-Id": str(state["session"])})
        if sid != str(state["session"]):
            return self.reply(404, {"error": "session expired"})
        if "id" not in body:
            return self.reply(202, {})
        if method == "tools/list":
            names = ["flip"] + (["new_tool"] if state["generation"] else [])
            result = {"tools": [{"name": n, "description": "fixture " + n, "inputSchema": {"type": "object", "properties": ({"value": {"type": "string"}} if n == "new_tool" else {"action": {"type": "string", "enum": ["read", "invalid"]}}), "required": (["value"] if n == "new_tool" else [])}, "outputSchema": {"type": "object", "properties": {"answer": {"type": "string"}}, "required": ["answer"]}} for n in names]}
        elif method == "tools/call":
            name = body.get("params", {}).get("name")
            arguments = body.get("params", {}).get("arguments", {})
            if name == "flip" and state["generation"] == 0 and not arguments.get("action"):
                state["generation"] = 1
                state["session"] += 1
            invalid = arguments.get("value") == "invalid" or arguments.get("action") == "invalid"
            answer = 42 if invalid else ("new-tool-ok" if name == "new_tool" else "flip-ok")
            result = {"content": [{"type": "text", "text": "fixture:" + str(name)}], "structuredContent": {"answer": answer}}
        elif method == "ping":
            result = {}
        else:
            result = {"tools": [], "resources": [], "resourceTemplates": [], "prompts": []}
        self.reply(200, {"jsonrpc": "2.0", "id": body["id"], "result": result})


ThreadingHTTPServer(("127.0.0.1", int(os.environ.get("PORT", "39811"))), Handler).serve_forever()
