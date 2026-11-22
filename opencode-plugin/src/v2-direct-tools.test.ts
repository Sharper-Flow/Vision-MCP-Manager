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

type ServerConfig = { disabled?: boolean }
type McpEditor = {
  get: (name: string) => ServerConfig | undefined
  update: (name: string, edit: (config: ServerConfig) => void) => void
}
type RegisteredTool = { name: string; execute: (raw: unknown) => Promise<{ content: string }> }

// Models the 2.3.2 host: every MCP rebuild starts from a fresh copy of the
// config and replays transforms in registration order; the tool list is
// rebuilt on read.
function replayingHost(config: Record<string, ServerConfig>) {
  const mcpTransforms: ((editor: McpEditor) => void)[] = []
  const toolTransforms: ((editor: { add: (tool: RegisteredTool) => void }) => void)[] = []
  let servers = new Map<string, ServerConfig>()
  const rebuildMcp = () => {
    servers = new Map(Object.entries(structuredClone(config)))
    const editor: McpEditor = {
      get: (name) => servers.get(name),
      update: (name, edit) => {
        const current = servers.get(name)
        if (current) edit(current)
      },
    }
    for (const transform of mcpTransforms) transform(editor)
  }
  const registration = { dispose: async () => {} }
  const context = {
    mcp: {
      transform: async (callback: (editor: McpEditor) => void) => {
        mcpTransforms.push(callback)
        rebuildMcp()
        return registration
      },
      reload: async () => rebuildMcp(),
      list: async () => ({
        data: [...servers].map(([name, server]) => ({
          name,
          status: { status: server.disabled === true ? "disabled" : "connected" },
        })),
      }),
    },
    tool: {
      transform: async (callback: (typeof toolTransforms)[number]) => {
        toolTransforms.push(callback)
        return registration
      },
    },
  }
  const tools = () => {
    const added: RegisteredTool[] = []
    for (const transform of toolTransforms) transform({ add: (tool) => added.push(tool) })
    return added
  }
  return { config, context, tools, rebuildMcp }
}

const names = (tools: RegisteredTool[]) => tools.map((tool) => tool.name).sort()
const pluginLocal = Object.values(OPENCODE_MCP_TOOL_NAMES).sort()
const all = Object.values(VISION_PLUGIN_TOOL_NAMES).sort()

it("follows the merged vision entry, including edits from later MCP transforms", async () => {
  const host = replayingHost({ vision: {} })
  await VisionPlugin.setup(host.context as never)
  expect(names(host.tools())).toEqual(pluginLocal)

  const call = async (name: string) => {
    const tool = host.tools().find((candidate) => candidate.name === name)
    return JSON.parse((await tool!.execute({ name: "vision" })).content)
  }
  expect(await call(OPENCODE_MCP_TOOL_NAMES.mcpDisconnect)).toMatchObject({ success: true })
  expect(names(host.tools())).toEqual(all)

  expect(await call(OPENCODE_MCP_TOOL_NAMES.mcpConnect)).toMatchObject({ success: true })
  expect(names(host.tools())).toEqual(pluginLocal)
})

it("follows a config reload that disables the vision server", async () => {
  const host = replayingHost({ vision: {} })
  await VisionPlugin.setup(host.context as never)
  expect(names(host.tools())).toEqual(pluginLocal)

  host.config.vision = { disabled: true }
  host.rebuildMcp()
  expect(names(host.tools())).toEqual(all)
})
