import { afterEach, describe, expect, it, vi } from "vitest"
import VisionPlugin from "./index"
import {
  OPENCODE_MCP_TOOL_NAMES,
  VISION_DAEMON_TOOL_NAMES,
  VISION_PLUGIN_TOOL_NAMES,
} from "./tool-names"
import { listTools } from "./mcp-client"

vi.mock("./health", () => ({
  checkHealth: vi.fn().mockResolvedValue({ healthy: false }),
}))

const daemonReachable = await fetch("http://localhost:6275/health")
  .then((response) => response.ok)
  .catch(() => false)

describe("Vision plugin ↔ daemon tool parity", () => {
  afterEach(() => {
    vi.unstubAllEnvs()
  })

  it("keeps daemon and plugin-local tool names disjoint", () => {
    const daemonToolNames = new Set<string>(Object.values(VISION_DAEMON_TOOL_NAMES))

    expect(
      Object.values(OPENCODE_MCP_TOOL_NAMES).filter((name) => daemonToolNames.has(name))
    ).toEqual([])
  })

  it.each([
    {
      codeMode: false,
      label: "Code Mode OFF",
      expected: Object.values(VISION_PLUGIN_TOOL_NAMES) as string[],
    },
    {
      codeMode: true,
      label: "Code Mode ON",
      expected: Object.values(OPENCODE_MCP_TOOL_NAMES) as string[],
    },
  ])(
    "keeps every registered plugin tool represented in the plugin name constants ($label)",
    async ({ codeMode, expected }) => {
      // Code Mode is read at factory time, so stub before constructing.
      vi.stubEnv("OPENCODE_EXPERIMENTAL_CODE_MODE", codeMode ? "true" : "false")

      const plugin = await VisionPlugin({ client: {} } as Parameters<typeof VisionPlugin>[0])
      // Read the `tool` record OpenCode actually consumes. No `?? {}` fallback:
      // a missing registration must fail here rather than silently compare an
      // empty list against an empty list.
      const registeredToolNames = Object.keys(
        (plugin as unknown as { tool: Record<string, unknown> }).tool
      )

      expect(registeredToolNames.sort()).toEqual([...expected].sort())
    }
  )

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
