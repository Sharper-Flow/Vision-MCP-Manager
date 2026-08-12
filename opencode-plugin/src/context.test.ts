import { describe, expect, it } from "vitest"
import { renderVisionContext } from "./context"
import { OPENCODE_MCP_TOOL_NAMES, VISION_DAEMON_TOOL_NAMES } from "./tool-names"

describe("renderVisionContext", () => {
  const states = [
    { healthy: true, codeMode: true },
    { healthy: true, codeMode: false },
    { healthy: false, codeMode: true },
    { healthy: false, codeMode: false },
  ] as const

  it.each(states)("includes OpenCode MCP tools in %j", (state) => {
    const context = renderVisionContext(state)

    for (const name of Object.values(OPENCODE_MCP_TOOL_NAMES)) {
      expect(context).toContain(name)
    }
  })

  it.each([
    { healthy: true, codeMode: true },
    { healthy: true, codeMode: false },
  ] as const)("includes every daemon tool in healthy %j", (state) => {
    const context = renderVisionContext(state)

    for (const name of Object.values(VISION_DAEMON_TOOL_NAMES)) {
      const renderedName = state.codeMode ? `tools.vision.${name}()` : name
      expect(context).toContain(renderedName)
    }
  })

  it.each([true, false] as const)(
    "describes unavailable daemon-backed tools when daemon is down (Code Mode: %s)",
    (codeMode) => {
      const context = renderVisionContext({ healthy: false, codeMode })

      expect(context).toContain("Vision daemon is NOT running")
      expect(context).toContain("daemon-backed tools are unavailable")
    }
  )

  it.each(states)("does not use retired capability claims in %j", (state) => {
    const context = renderVisionContext(state)

    expect(context).not.toContain("MCP server management is unavailable")
    expect(context).not.toContain("unavailable in Code Mode")
  })

  it("does not render OpenCode MCP tools in the Vision Code Mode namespace", () => {
    for (const healthy of [true, false]) {
      const context = renderVisionContext({ healthy, codeMode: true })

      expect(context).not.toContain("tools.vision.opencode_mcp_")
    }
  })
})
