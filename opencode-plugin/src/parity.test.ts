import { afterEach, describe, expect, it, vi } from "vitest"
import VisionPlugin from "./index"
import {
  OPENCODE_MCP_TOOL_NAMES,
  VISION_DAEMON_TOOL_NAMES,
  VISION_PLUGIN_TOOL_NAMES,
} from "./tool-names"
import { listTools } from "./mcp-client"
import { daemonReachable } from "./daemon-gate"

vi.mock("./health", () => ({
  checkHealth: vi.fn().mockResolvedValue({ healthy: false }),
}))

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

      // The dual entry carries the V1 factory on `server`.
      const plugin = await VisionPlugin.server({
        client: {},
      } as unknown as Parameters<(typeof VisionPlugin)["server"]>[0])
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

// =============================================================================
// V1 ↔ V2 registration parity
// =============================================================================
//
// The dual entry registers the same surface twice — once as zod-args `tool`
// hooks for 1.18.x, once as JSON Schema `editor.add` calls for V2. These tests
// lock the two registrations together so a drift in names or descriptions on
// either side fails instead of shipping.

interface RecordedV2Tool {
  name: string
  description: string
}

async function registerV2Tools(codeMode: boolean): Promise<Record<string, RecordedV2Tool>> {
  vi.stubEnv("OPENCODE_EXPERIMENTAL_CODE_MODE", codeMode ? "true" : "false")

  const added: Record<string, RecordedV2Tool> = {}
  const context = {
    location: { directory: "/tmp/project" },
    event: {
      subscribe: () => (async function* () {})(),
    },
    session: {
      hook: async () => ({ dispose: async () => {} }),
    },
    tool: {
      transform: async (callback: (editor: unknown) => void) => {
        callback({
          add: (tool: RecordedV2Tool) => {
            added[tool.name] = tool
          },
        })
        return { dispose: async () => {} }
      },
    },
    mcp: {
      transform: async () => ({ dispose: async () => {} }),
      reload: async () => {},
      list: async () => ({ location: { directory: "/tmp/project" }, data: [] }),
    },
  }

  await (VisionPlugin as unknown as { setup: (context: unknown) => Promise<unknown> }).setup(
    context
  )
  return added
}

async function registerV1Tools(
  codeMode: boolean
): Promise<Record<string, { description: string }>> {
  vi.stubEnv("OPENCODE_EXPERIMENTAL_CODE_MODE", codeMode ? "true" : "false")

  const plugin = await VisionPlugin.server({
    client: {},
  } as unknown as Parameters<(typeof VisionPlugin)["server"]>[0])
  return (plugin as unknown as { tool: Record<string, { description: string }> }).tool
}

describe("V1 ↔ V2 registration parity", () => {
  afterEach(() => {
    vi.unstubAllEnvs()
  })

  it.each([false, true])(
    "registers the same tool names in both loaders (Code Mode %s)",
    async (codeMode) => {
      const v1 = await registerV1Tools(codeMode)
      const v2 = await registerV2Tools(codeMode)

      expect(Object.keys(v2).sort()).toEqual(Object.keys(v1).sort())
    }
  )

  it("keeps V1 and V2 descriptions identical for every tool", async () => {
    const v1 = await registerV1Tools(false)
    const v2 = await registerV2Tools(false)

    for (const [name, definition] of Object.entries(v1)) {
      expect(v2[name], `${name} missing from V2 registration`).toBeDefined()
      expect(v2[name].description, `${name} description drifted between loaders`).toBe(
        definition.description
      )
    }
  })
})
