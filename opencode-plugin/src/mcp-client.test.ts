/**
 * MCP client session lifecycle tests.
 *
 * Integration coverage for callTool: it must perform the MCP initialize
 * handshake, then round-trip a tools/call and return the tool's payload.
 * Requires a live daemon on localhost:6275. Skipped without one locally;
 * in CI the shared gate in ./daemon-gate refuses instead of skipping.
 */
import { describe, it, expect } from "vitest"
import { callTool } from "./mcp-client"
import { daemonReachable } from "./daemon-gate"

describe("MCP client session lifecycle (integration, live daemon required)", () => {
  it.runIf(daemonReachable)(
    "callTool round-trips vision_status against the live daemon",
    async () => {
      const result = await callTool("vision_status", {})
      const parsed = JSON.parse(result)

      // callTool resolves with { success: false, error, code } instead of
      // rejecting, so a failed round-trip must be caught explicitly.
      expect(parsed.error).toBeUndefined()

      // vision_status returns StatusResponse. `healthy` is a readiness flag: it
      // is false whenever any registered server is not running, including one
      // deliberately configured autostart:false. Assert the payload shape the
      // round-trip must produce, not a particular health verdict.
      expect(typeof parsed.healthy).toBe("boolean")
      expect(typeof parsed.uptime).toBe("string")
      expect(typeof parsed.memory_mb).toBe("number")
      expect(typeof parsed.servers.running).toBe("number")
      expect(typeof parsed.servers.stopped).toBe("number")
      expect(typeof parsed.servers.error).toBe("number")
    },
    30_000
  )
})
