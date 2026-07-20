/**
 * MCP client session lifecycle tests.
 *
 * RED phase: integration test asserting callTool("vision_status", {}) round-trips
 * against the live daemon. Current behavior is BROKEN — the plugin's mcp-client.ts
 * POSTs tools/call without first performing the MCP initialize handshake, and the
 * daemon rejects with `method "tools/call" is invalid during session initialization`.
 */
import { describe, it, expect } from "vitest"
import { callTool } from "./mcp-client"

const daemonReachable = await fetch("http://localhost:6275/health")
  .then((response) => response.ok)
  .catch(() => false)

describe("MCP client session lifecycle (integration, live daemon required)", () => {
  it.runIf(daemonReachable)(
    "callTool round-trips vision_status against the live daemon",
    async () => {
      const result = await callTool("vision_status", {})
      const parsed = JSON.parse(result)
      // vision_status returns { healthy: true, uptime, servers, memory_mb, warnings }
      expect(parsed.healthy).toBe(true)
    },
    30_000
  )
})
