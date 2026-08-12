/**
 * Registration guard.
 *
 * Every other test in this suite either calls a handler function directly
 * (mcp-control.test.ts, tools tests) or compares name constants
 * (parity.test.ts). None of them prove the tools are actually reachable by
 * OpenCode. All twelve could be unregistered with a fully green suite — and
 * that is exactly what happened: the plugin returned `tools: [...]` (an array)
 * while `Hooks` declares `tool?: { [key: string]: ToolDefinition }`, so
 * OpenCode silently registered nothing.
 *
 * Typecheck does not cover it either: injecting a bogus excess property into
 * the same return object produces zero tsc errors.
 *
 * This file is the structural guard (P33) that makes a registration failure
 * impossible to ship green.
 */

import { describe, it, expect } from "vitest"
import VisionPlugin from "./index"
import { VISION_PLUGIN_TOOL_NAMES } from "./tool-names"

/**
 * Minimal PluginInput stand-in. The mcp methods are stubs that throw: nothing
 * in registration may call the client, and this asserts it by construction
 * rather than by convention. No network, no daemon.
 */
function fakePluginInput() {
  const fail = (method: string) => async () => {
    throw new Error(`client.mcp.${method} must not be called during registration`)
  }
  return {
    client: {
      mcp: {
        status: fail("status"),
        connect: fail("connect"),
        disconnect: fail("disconnect"),
      },
    },
    project: {},
    directory: "/tmp",
    worktree: "/tmp",
    experimental_workspace: { register: () => {} },
    serverUrl: new URL("http://localhost:4096"),
    $: {},
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
  } as any
}

// eslint-disable-next-line @typescript-eslint/no-explicit-any
async function loadHooks(): Promise<any> {
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  return await (VisionPlugin as any)(fakePluginInput())
}

const expectedNames = Object.values(VISION_PLUGIN_TOOL_NAMES) as string[]

describe("plugin tool registration", () => {
  it("exposes a `tool` record, not a `tools` array", async () => {
    const hooks = await loadHooks()

    expect(
      hooks.tool,
      "Hooks.tool is missing. OpenCode reads `tool` (a record keyed by tool name); " +
        "a `tools` array is not part of the Hooks contract and is silently ignored.",
    ).toBeDefined()
    expect(Array.isArray(hooks.tool)).toBe(false)
    expect(typeof hooks.tool).toBe("object")
  })

  it("does not carry a legacy `tools` key", async () => {
    const hooks = await loadHooks()
    // Locks out regression to the array shape.
    expect(hooks.tools).toBeUndefined()
  })

  it("registers exactly the declared tool names", async () => {
    const hooks = await loadHooks()
    const registered = Object.keys(hooks.tool ?? {})

    // Bidirectional: a name added to the constant but never registered fails,
    // and a tool registered under an undeclared name fails too.
    expect(new Set(registered)).toEqual(new Set(expectedNames))
    expect(registered).toHaveLength(expectedNames.length)
  })

  it.each(expectedNames)("%s satisfies the ToolDefinition shape", async (name) => {
    const hooks = await loadHooks()
    const def = (hooks.tool ?? {})[name]

    expect(def, `tool "${name}" is not registered`).toBeDefined()

    expect(typeof def.description).toBe("string")
    expect(def.description.length).toBeGreaterThan(0)

    expect(typeof def.execute).toBe("function")

    // `args` must be a bare ZodRawShape — a plain object whose values are Zod
    // schemas — NOT a wrapped ZodObject. A ZodObject would expose _def/parse.
    expect(def.args, `tool "${name}" has no args`).toBeDefined()
    expect(typeof def.args).toBe("object")
    expect(
      (def.args as { _def?: unknown })._def,
      `tool "${name}" passes a wrapped ZodObject; ToolDefinition wants a ZodRawShape (use .shape)`,
    ).toBeUndefined()
    expect((def.args as { parse?: unknown }).parse).toBeUndefined()

    for (const [key, schema] of Object.entries(def.args as Record<string, unknown>)) {
      expect(
        (schema as { _def?: unknown })?._def,
        `arg "${key}" of tool "${name}" is not a Zod schema`,
      ).toBeDefined()
    }
  })

  it("registers the two opencode_mcp_* tools with their contract language intact", async () => {
    const hooks = await loadHooks()

    for (const name of ["opencode_mcp_connect", "opencode_mcp_disconnect"]) {
      const def = (hooks.tool ?? {})[name]
      expect(def, `${name} is not registered`).toBeDefined()
      // These two descriptions carry the next-turn availability and
      // session-only non-persistence contract the agent relies on.
      expect(def.description).toContain("following turn")
      expect(def.description).toContain("does not persist")
    }
  })
})
