import { describe, expect, it } from "vitest"
import { VISION_PLUGIN_TOOL_NAMES } from "./index"
import { listTools } from "./mcp-client"

const daemonReachable = await fetch("http://localhost:6275/health")
  .then((response) => response.ok)
  .catch(() => false)

describe("Vision plugin ↔ daemon tool parity", () => {
  it.runIf(daemonReachable)("wraps every tool served by the live daemon", async () => {
    const daemonTools = (await listTools()).sort()
    const pluginTools = Object.values(VISION_PLUGIN_TOOL_NAMES).sort()

    expect(pluginTools).toEqual(daemonTools)
  })
})
