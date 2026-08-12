import { describe, expect, it } from "vitest"
import { OPENCODE_MCP_TOOL_NAMES, VISION_DAEMON_TOOL_NAMES } from "./tool-names"
import { listTools } from "./mcp-client"

const daemonReachable = await fetch("http://localhost:6275/health")
  .then((response) => response.ok)
  .catch(() => false)

describe("Vision plugin ↔ daemon tool parity", () => {
  it("keeps daemon and plugin-local tool names disjoint", () => {
    const daemonToolNames = new Set<string>(Object.values(VISION_DAEMON_TOOL_NAMES))

    expect(
      Object.values(OPENCODE_MCP_TOOL_NAMES).filter((name) => daemonToolNames.has(name))
    ).toEqual([])
  })

  it.runIf(daemonReachable)("wraps every tool served by the live daemon", async () => {
    expect(Object.values(VISION_DAEMON_TOOL_NAMES).sort()).toEqual((await listTools()).sort())
  })

  it.runIf(daemonReachable)("keeps plugin-local tools out of the live daemon", async () => {
    const daemonTools = await listTools()

    expect(Object.values(OPENCODE_MCP_TOOL_NAMES).some((name) => daemonTools.includes(name))).toBe(
      false
    )
  })
})
