import { afterEach, expect, it, vi } from "vitest"
import VisionPlugin from "./index"
import * as rootEntry from "../index"
import { OPENCODE_MCP_TOOL_NAMES, VISION_PLUGIN_TOOL_NAMES } from "./tool-names"

afterEach(() => vi.unstubAllEnvs())

it("exports the same plugin object from the V2 directory entry", () => {
  expect(Object.keys(rootEntry)).toEqual(["default"])
  expect(rootEntry.default).toBe(VisionPlugin)
})

it.each([
  { label: "no vision server", server: undefined, env: "false", all: true },
  { label: "enabled vision server", server: {}, env: "false", all: false },
  { label: "direct vision server", server: { codemode: false }, env: "false", all: false },
  { label: "disabled vision server", server: { disabled: true }, env: "false", all: true },
  { label: "environment only", server: undefined, env: "true", all: true },
])("registers V2 tools for $label", async ({ server, env, all }) => {
  vi.stubEnv("OPENCODE_EXPERIMENTAL_CODE_MODE", env)
  const tools: { name: string; options?: { codemode?: boolean } }[] = []
  const context = {
    mcp: {
      transform: async (edit: (editor: unknown) => void) =>
        edit({ get: (name: string) => (name === "vision" ? server : undefined) }),
    },
    tool: {
      transform: async (edit: (editor: unknown) => void) =>
        edit({ add: (tool: unknown) => tools.push(tool as (typeof tools)[number]) }),
    },
  }
  await VisionPlugin.setup(context as never)
  expect(tools.map((tool) => tool.name).sort()).toEqual(
    Object.values(all ? VISION_PLUGIN_TOOL_NAMES : OPENCODE_MCP_TOOL_NAMES).sort()
  )
  for (const tool of tools) expect(tool.options?.codemode, tool.name).toBe(false)
})

it("updates daemon tools only when the MCP transform captures the new server state", async () => {
  const server = { disabled: false }
  let mcpTransform = () => {}
  let toolTransform = () => {}
  let tools: string[] = []
  const context = {
    mcp: {
      transform: async (edit: (editor: unknown) => void) => {
        mcpTransform = () => edit({ get: () => server })
        mcpTransform()
      },
    },
    tool: {
      transform: async (edit: (editor: unknown) => void) => {
        toolTransform = () => edit({ add: (tool: { name: string }) => tools.push(tool.name) })
        toolTransform()
      },
    },
  }

  await VisionPlugin.setup(context as never)
  expect(tools.sort()).toEqual(Object.values(OPENCODE_MCP_TOOL_NAMES).sort())

  server.disabled = true
  tools = []
  toolTransform()
  expect(tools.sort()).toEqual(Object.values(OPENCODE_MCP_TOOL_NAMES).sort())

  mcpTransform()
  tools = []
  toolTransform()
  expect(tools.sort()).toEqual(Object.values(VISION_PLUGIN_TOOL_NAMES).sort())
})
