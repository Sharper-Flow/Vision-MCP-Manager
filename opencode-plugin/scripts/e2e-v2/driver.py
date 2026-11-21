#!/usr/bin/env python3
"""Private-sandbox driver for the checks the OpenCode V2 lab cannot express:
the HTTP MCP session-expiry probe, the reconnect probe, the directory
loader probes, the read-only repository bind, and the opt-in V1 checks."""

import json
import os
import subprocess
import sys
import time
import urllib.request

root = os.environ["FIXTURE"]
env = os.environ.copy()
env.update({"HOME": root, "XDG_CONFIG_HOME": root + "/config", "XDG_DATA_HOME": root + "/data", "XDG_CACHE_HOME": root + "/cache", "XDG_STATE_HOME": root + "/state", "OPENCODE_DISABLE_AUTOUPDATE": "true"})
config = {
    "plugins": [{"package": root + "/install/node_modules/vision-plugin"}],
    "mcp": {"servers": {"fixture": {"type": "remote", "url": "http://127.0.0.1:39811/mcp", "oauth": False}}},
    "model": "stub/m",
    "provider": {"stub": {"npm": "@ai-sdk/openai-compatible", "name": "stub", "options": {"baseURL": "http://127.0.0.1:39811/v1", "apiKey": "stub"}, "models": {"m": {"name": "m"}}}},
    "permission": {"*": "allow"},
}

expected_daemon = {"vision_list", "vision_remove", "vision_restart", "vision_init", "vision_status", "vision_guidance", "vision_slot_status", "vision_metrics"}
expected_control = {"opencode_mcp_connect", "opencode_mcp_disconnect"}
failures = []


def check(label, condition, detail=""):
    print(("OK: " if condition else "FAIL: ") + label + (" " + detail if detail else ""), flush=True)
    if not condition:
        failures.append(label)


def structural_check():
    probe = subprocess.run([sys.executable, root + "/probe.py"], capture_output=True, text=True, timeout=10)
    check("structural live paths hidden", probe.returncode == 0, probe.stdout.strip())


def tool_results(requests):
    if not requests:
        return []
    messages = requests[-1].get("messages", [])
    return [m.get("content", "") for m in messages if m.get("role") == "tool"]


def run(label, flag, setting, prompt, server_codemode=True, plugin_path=None, v1=False):
    urllib.request.urlopen("http://127.0.0.1:39811/reset", timeout=2).read()
    if label == "reconnect":
        urllib.request.urlopen("http://127.0.0.1:39811/reconnect-probe", timeout=2).read()
    cfg = json.loads(json.dumps(config))
    if v1:
        cfg.pop("plugins")
        cfg["plugin"] = [plugin_path]
        cfg["mcp"] = {"fixture": {"type": "remote", "url": "http://127.0.0.1:39811/mcp", "enabled": True, "oauth": False}}
    else:
        if plugin_path is not None:
            cfg["plugins"] = [{"package": plugin_path}]
        cfg["mcp"]["servers"]["fixture"]["codemode"] = server_codemode
        if setting:
            cfg["experimental"] = {"full_codemode": True}
    path = root + "/opencode.json"
    with open(path, "w", encoding="utf-8") as out:
        json.dump(cfg, out)
    run_env = dict(env)
    if v1:
        run_env.update({"HOME": root + "/v1home", "XDG_CONFIG_HOME": root + "/v1home/config", "XDG_DATA_HOME": root + "/v1home/data", "XDG_CACHE_HOME": root + "/v1home/cache", "XDG_STATE_HOME": root + "/v1home/state"})
        os.makedirs(run_env["HOME"], exist_ok=True)
    if flag:
        run_env["OPENCODE_EXPERIMENTAL_CODE_MODE"] = "true"
    else:
        run_env.pop("OPENCODE_EXPERIMENTAL_CODE_MODE", None)
    req = root + "/requests.jsonl"
    before = os.path.getsize(req) if os.path.exists(req) else 0
    try:
        binary = root + ("/opencode-v1" if v1 else "/opencode")
        standalone = [] if v1 else ["--standalone"]
        debug = ["--log-level", "DEBUG"] if v1 else []
        process = subprocess.run(["timeout", "--kill-after=5s", "90s", binary, "run", *standalone, "--print-logs", *debug, "--model", "stub/m", prompt], env=run_env, cwd=root, capture_output=True, timeout=100)
        code = process.returncode
        output = process.stdout + b"\n--- stderr ---\n" + process.stderr
    except subprocess.TimeoutExpired as exc:
        code = 124
        output = (exc.stdout or b"") + b"\n--- stderr ---\n" + (exc.stderr or b"")
    with open(root + "/" + label + ".log", "wb") as out:
        out.write(output)
    requests = []
    if os.path.exists(req):
        with open(req, "rb") as inp:
            inp.seek(before)
            requests = [json.loads(line) for line in inp if line.strip()]
    with open(root + "/" + label + ".json", "w", encoding="utf-8") as out:
        json.dump(requests, out, indent=2)
    tool_requests = [r for r in requests if r.get("tools")]
    names = [t.get("function", {}).get("name") for t in tool_requests[0].get("tools", [])] if tool_requests else []
    print("MATRIX %s exit=%s requests=%s tools=%s" % (label, code, len(requests), names), flush=True)
    return code, requests, names


def loader_probes():
    scenarios = {
        "index-js": (["index.js"], {}),
        "index-ts": (["index.ts"], {}),
        "index-mjs": (["index.mjs"], {}),
        "index-tsx": (["index.tsx"], {}),
        "server-js": (["server.js"], {}),
        "server-ts": (["server.ts"], {}),
        "server-mjs": (["server.mjs"], {}),
        "server-tsx": (["server.tsx"], {}),
        "server-v-index": (["server.js", "index.js"], {}),
        "server-js-v-index-ts": (["server.js", "index.ts"], {}),
        "js-v-ts": (["index.js", "index.ts"], {}),
        "all-four": (["server.js", "server.ts", "index.js", "index.ts"], {}),
        "main-only": (["custom.js"], {"main": "./custom.js"}),
        "exports-only": (["custom.js"], {"exports": {".": "./custom.js", "./server": "./custom.js"}}),
    }
    for name, (files, manifest) in scenarios.items():
        directory = root + "/probe-" + name
        os.makedirs(directory, exist_ok=True)
        with open(directory + "/package.json", "w", encoding="utf-8") as out:
            json.dump({"type": "module", **manifest}, out)
        for file in files:
            marker = "probe_" + file.replace(".", "_")
            module = 'export default { id: "' + marker + '", async setup(ctx) { await ctx.tool.transform(e => e.add({name: "' + marker + '", description: "probe", input: {type: "object", properties: {}}, options: {codemode: false}, execute: async () => ({content: "ok"})})) } };\n'
            with open(directory + "/" + file, "w", encoding="utf-8") as out:
                out.write(module)
        code, _, names = run("loader-" + name, False, False, "matrix probe", False, directory)
        print("LOADER %s exit=%s loaded=%s" % (name, code, [n for n in names if n and n.startswith("probe_")]), flush=True)


server = subprocess.Popen([sys.executable, root + "/server.py"], env=env)
try:
    for _ in range(100):
        try:
            urllib.request.urlopen("http://127.0.0.1:39811/health", timeout=0.2).read()
            break
        except OSError:
            time.sleep(0.1)
    else:
        raise RuntimeError("fixture server did not start")
    structural_check()
    if os.environ.get("LOADER_PROBE") == "1":
        loader_probes()
    code, _, names = run("repo-root-readonly", False, False, "matrix probe", False, root + "/repo-plugin")
    check("repo-root-readonly V2 setup", code == 0 and expected_control <= set(names) and expected_daemon <= set(names))
    reconnect_code, reconnect_requests, _ = run("reconnect", True, True, "reconnect probe")
    reconnect_results = tool_results(reconnect_requests)
    for index, result in enumerate(reconnect_results):
        print("RECONNECT result %d: %s" % (index + 1, result), flush=True)
    current = reconnect_code == 0 and len(reconnect_results) >= 3 and "tools.fixture.new_tool" in reconnect_results[2] and "new-tool-ok" in reconnect_results[2]
    print("RECONNECT catalog: " + ("current" if current else "incomplete or stale"), flush=True)
    _, requests, _ = run("expiry", True, True, "expiry probe")
    results = tool_results(requests)
    for index, result in enumerate(results):
        if index == 4:
            try:
                found = json.loads(result)
                result = json.dumps([item for item in found.get("items", []) if item.get("path") == "tools.fixture.new_tool"])
            except (json.JSONDecodeError, AttributeError):
                pass
        print("EXPIRY result %d: %s" % (index + 1, result), flush=True)
    with open(root + "/expiry.log", encoding="utf-8") as inp:
        log = inp.read()
    reconnected = "mcp session expired, reconnecting" in log and "server=fixture tools=2" in log
    print("EXPIRY reconnect observed: " + str(reconnected), flush=True)
    if reconnected and len(results) >= 7 and "tools.fixture.new_tool" in results[4]:
        for label, index, expected in (("pre-expiry invalid", 0, '"answer":42'), ("post-expiry valid", 5, '"answer":"new-tool-ok"'), ("post-expiry invalid", 6, '"answer":42')):
            accepted = expected in results[index].replace(" ", "")
            print("EXPIRY %s: %s; result=%s" % (label, "accepted" if accepted else "rejected or unrecognized", results[index]), flush=True)
    else:
        print("EXPIRY: incomplete or stale (see expiry.json and mcp.jsonl)", flush=True)
    if os.environ.get("V1_ENABLED") == "1":
        v1env = dict(env)
        v1env.update({"HOME": root + "/v1home", "XDG_CONFIG_HOME": root + "/v1home/config", "XDG_DATA_HOME": root + "/v1home/data", "XDG_CACHE_HOME": root + "/v1home/cache", "XDG_STATE_HOME": root + "/v1home/state"})
        os.makedirs(v1env["HOME"], exist_ok=True)
        version = subprocess.run(["timeout", "15s", root + "/opencode-v1", "--version"], env=v1env, cwd=root, capture_output=True, text=True, timeout=20)
        check("V1 host version", version.returncode == 0 and version.stdout.strip() == "1.18.35", repr(version.stdout.strip()))
        if version.returncode == 0 and version.stdout.strip() == "1.18.35":
            for source, path in (("installed", root + "/install/node_modules/vision-plugin"), ("repo-readonly", root + "/repo-plugin")):
                for mode in (False, True):
                    label = "v1-%s-%s" % (source, "on" if mode else "off")
                    code, requests, names = run(label, mode, False, "control probe", plugin_path=path, v1=True)
                    names = set(names)
                    check(label + " host completed", code == 0 and bool(requests))
                    check(label + " direct control tools", expected_control <= names, "missing=" + repr(sorted(expected_control - names)))
                    if mode:
                        check(label + " daemon tools suppressed", not any(name.startswith("vision_") for name in names if name))
                    else:
                        check(label + " eight daemon tools", expected_daemon <= names, "missing=" + repr(sorted(expected_daemon - names)))
                    results = tool_results(requests)
                    for index, (description, status) in enumerate((("disconnect", "disabled"), ("connect", "connected"))):
                        try:
                            value = json.loads(results[index])
                        except (IndexError, json.JSONDecodeError, TypeError):
                            value = {}
                        check(label + " " + description, value.get("success") is True and value.get("status") == status, repr(value))
    if failures:
        print("FAIL: " + str(len(failures)) + " qualification assertions", flush=True)
        sys.exit(1)
finally:
    server.terminate()
    server.wait(timeout=5)
