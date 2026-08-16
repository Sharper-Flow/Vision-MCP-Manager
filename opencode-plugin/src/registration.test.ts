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

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"
import { mkdir, writeFile } from "node:fs/promises"
import { join } from "node:path"
import { tmpdir } from "node:os"

const { checkHealthMock, visionInitMock } = vi.hoisted(() => ({
  checkHealthMock: vi.fn(async () => ({ healthy: false })),
  visionInitMock: vi.fn(async () => "mocked vision_init"),
}))

vi.mock("./health", async (importOriginal) => ({
  ...(await importOriginal<typeof import("./health")>()),
  checkHealth: checkHealthMock,
}))

vi.mock("./tools", async (importOriginal) => ({
  ...(await importOriginal<typeof import("./tools")>()),
  visionInit: visionInitMock,
}))

import VisionPlugin from "./index"
import {
  OPENCODE_MCP_TOOL_NAMES,
  VISION_DAEMON_TOOL_NAMES,
  VISION_PLUGIN_TOOL_NAMES,
} from "./tool-names"

/**
 * Minimal PluginInput stand-in. The mcp methods are stubs that throw: nothing
 * in registration may call the client, and this asserts it by construction
 * rather than by convention. No network, no daemon.
 */
function fakePluginInput(directory = "/tmp/project") {
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
    directory,
    worktree: directory,
    experimental_workspace: { register: () => {} },
    serverUrl: new URL("http://localhost:4096"),
    $: {},
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
  } as any
}

// eslint-disable-next-line @typescript-eslint/no-explicit-any
async function loadHooks(directory = "/tmp/project", codeMode = false): Promise<any> {
  // Registration reads Code Mode at FACTORY time, so the env must be set before
  // the factory runs — not after. Defaulting to OFF keeps every daemon-tool test
  // below deterministic regardless of how the suite was launched: dev sessions on
  // this host run with OPENCODE_EXPERIMENTAL_CODE_MODE=true, which would
  // otherwise unregister vision_init and friends out from under those tests.
  vi.stubEnv("OPENCODE_EXPERIMENTAL_CODE_MODE", codeMode ? "true" : "false")
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  return await (VisionPlugin as any)(fakePluginInput(directory))
}

async function tempProject(): Promise<string> {
  const directory = join(tmpdir(), `vision-plugin-registration-${Date.now()}-${Math.random()}`)
  await mkdir(directory, { recursive: true })
  return directory
}

const expectedNames = Object.values(VISION_PLUGIN_TOOL_NAMES) as string[]
const daemonNames = Object.values(VISION_DAEMON_TOOL_NAMES) as string[]
const pluginLocalNames = Object.values(OPENCODE_MCP_TOOL_NAMES) as string[]

/**
 * Under Code Mode the ten daemon-proxy tools are already reachable as
 * `tools.vision.*` from the `vision` MCP server, so registering them here
 * duplicates them — and plugin schemas, unlike MCP schemas, are NOT collapsed
 * by Code Mode. The two `opencode_mcp_*` tools have no Code Mode equivalent
 * (that catalog is built from MCP tools only) and must survive in both states.
 */
function expectedFor(codeMode: boolean): string[] {
  return codeMode ? pluginLocalNames : expectedNames
}

describe("plugin tool registration", () => {
  beforeEach(() => {
    checkHealthMock.mockClear()
    visionInitMock.mockClear()
  })

  afterEach(() => {
    vi.unstubAllEnvs()
  })

  it("exposes a `tool` record, not a `tools` array", async () => {
    const hooks = await loadHooks()

    expect(
      hooks.tool,
      "Hooks.tool is missing. OpenCode reads `tool` (a record keyed by tool name); " +
        "a `tools` array is not part of the Hooks contract and is silently ignored."
    ).toBeDefined()
    expect(Array.isArray(hooks.tool)).toBe(false)
    expect(typeof hooks.tool).toBe("object")
  })

  it("does not probe config files or initialize Vision during registration", async () => {
    const hooks = await loadHooks(await tempProject())

    expect(hooks).toBeDefined()
    expect(visionInitMock).not.toHaveBeenCalled()
  })

  it("does not carry a legacy `tools` key", async () => {
    const hooks = await loadHooks()
    // Locks out regression to the array shape.
    expect(hooks.tools).toBeUndefined()
  })

  it.each([
    { codeMode: false, label: "Code Mode OFF" },
    { codeMode: true, label: "Code Mode ON" },
  ])("registers exactly the declared tool names ($label)", async ({ codeMode }) => {
    const hooks = await loadHooks("/tmp/project", codeMode)
    const registered = Object.keys(hooks.tool ?? {})
    const expected = expectedFor(codeMode)

    // Bidirectional: a name added to the constant but never registered fails,
    // and a tool registered under an undeclared name fails too.
    expect(new Set(registered)).toEqual(new Set(expected))
    expect(registered).toHaveLength(expected.length)
  })

  it("omits the daemon-proxy tools under Code Mode", async () => {
    const hooks = await loadHooks("/tmp/project", true)
    const registered = Object.keys(hooks.tool ?? {})

    expect(
      registered.filter((name) => daemonNames.includes(name)),
      "daemon-proxy tools are reachable as tools.vision.* under Code Mode; " +
        "registering them here duplicates them at full plugin-schema cost"
    ).toEqual([])
  })

  it("keeps the opencode_mcp_* tools registered in BOTH Code Mode states", async () => {
    for (const codeMode of [false, true]) {
      const hooks = await loadHooks("/tmp/project", codeMode)
      const registered = Object.keys(hooks.tool ?? {})

      for (const name of pluginLocalNames) {
        expect(
          registered,
          `${name} must stay registered with Code Mode ${codeMode ? "ON" : "OFF"}: ` +
            "it has no Code Mode catalog entry, so suppressing it makes it unreachable"
        ).toContain(name)
      }
    }
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
      `tool "${name}" passes a wrapped ZodObject; ToolDefinition wants a ZodRawShape (use .shape)`
    ).toBeUndefined()
    expect((def.args as { parse?: unknown }).parse).toBeUndefined()

    for (const [key, schema] of Object.entries(def.args as Record<string, unknown>)) {
      expect(
        (schema as { _def?: unknown })?._def,
        `arg "${key}" of tool "${name}" is not a Zod schema`
      ).toBeDefined()
    }
  })

  it("registers the two opencode_mcp_* tools with their contract language intact", async () => {
    const hooks = await loadHooks()

    for (const name of ["opencode_mcp_connect", "opencode_mcp_disconnect"]) {
      const def = (hooks.tool ?? {})[name]
      expect(def, `${name} is not registered`).toBeDefined()
      // These two descriptions carry the availability and session-only
      // non-persistence contract the agent relies on.
      //
      // The availability pin is "takes effect immediately", not "following
      // turn". The original next-turn claim was measured false: disconnecting
      // a server removed it from the live tool surface within the same turn,
      // and reconnecting restored it and allowed a successful call in that
      // same turn. Only the turn's advertised tool list is fixed at turn start.
      expect(def.description).toContain("takes effect immediately")
      expect(def.description).toContain("does not persist")
      expect(
        def.description,
        `${name} still claims next-turn-only availability, which was measured false`
      ).not.toContain("become available on the following turn")
    }
  })

  it("forwards the plugin project config path when vision_init receives no path", async () => {
    const hooks = await loadHooks()
    const init = (hooks.tool ?? {})[VISION_PLUGIN_TOOL_NAMES.init]

    await init.execute({})

    expect(visionInitMock).toHaveBeenCalledWith(
      expect.objectContaining({ path: "/tmp/project/opencode.jsonc" })
    )
  })

  it.each([
    "opencode.jsonc",
    "opencode.json",
    ".opencode/opencode.jsonc",
    ".opencode/opencode.json",
  ])("selects the sole existing %s candidate", async (candidate) => {
    const directory = await tempProject()
    await mkdir(join(directory, ".opencode"), { recursive: true })
    await writeFile(join(directory, candidate), "{}\n")

    const hooks = await loadHooks(directory)
    await (hooks.tool ?? {})[VISION_PLUGIN_TOOL_NAMES.init].execute({})

    expect(visionInitMock).toHaveBeenCalledWith(
      expect.objectContaining({ path: join(directory, candidate) })
    )
  })

  it("keeps the documented root opencode.jsonc fallback when no candidate exists", async () => {
    const directory = await tempProject()
    const hooks = await loadHooks(directory)
    await (hooks.tool ?? {})[VISION_PLUGIN_TOOL_NAMES.init].execute({})

    expect(visionInitMock).toHaveBeenCalledWith(
      expect.objectContaining({ path: join(directory, "opencode.jsonc") })
    )
  })

  it("rejects ambiguous config candidates without calling vision_init", async () => {
    const directory = await tempProject()
    await writeFile(join(directory, "opencode.jsonc"), "{}\n")
    await writeFile(join(directory, "opencode.json"), "{}\n")
    const hooks = await loadHooks(directory)

    await expect((hooks.tool ?? {})[VISION_PLUGIN_TOOL_NAMES.init].execute({})).rejects.toThrow(
      new RegExp("opencode\\.jsonc.*opencode\\.json|opencode\\.json.*opencode\\.jsonc")
    )
    expect(visionInitMock).not.toHaveBeenCalled()
  })

  it("resolves a relative explicit path under the plugin directory", async () => {
    const directory = await tempProject()
    const hooks = await loadHooks(directory)
    await (hooks.tool ?? {})[VISION_PLUGIN_TOOL_NAMES.init].execute({ path: "nested/config.jsonc" })

    expect(visionInitMock).toHaveBeenCalledWith(
      expect.objectContaining({ path: join(directory, "nested/config.jsonc") })
    )
  })

  it("preserves an explicitly supplied vision_init path", async () => {
    const hooks = await loadHooks()
    const init = (hooks.tool ?? {})[VISION_PLUGIN_TOOL_NAMES.init]

    await init.execute({ path: "/custom/opencode.json" })

    expect(visionInitMock).toHaveBeenCalledWith(
      expect.objectContaining({ path: "/custom/opencode.json" })
    )
  })

  it("keeps vision_init path optional without embedding a schema default", async () => {
    const hooks = await loadHooks()
    const init = (hooks.tool ?? {})[VISION_PLUGIN_TOOL_NAMES.init]
    const parsed = (
      init.args as {
        path: { safeParse: (value: unknown) => { success: boolean; data?: unknown } }
      }
    ).path.safeParse(undefined)

    expect(parsed.success).toBe(true)
    expect(parsed.data).toBeUndefined()
  })

  it("describes the recognized OpenCode config path", async () => {
    const hooks = await loadHooks()
    const init = (hooks.tool ?? {})[VISION_PLUGIN_TOOL_NAMES.init]

    expect(init.description).not.toContain(".opencode.json")
    expect(init.description).toContain("opencode.jsonc")
  })
})
