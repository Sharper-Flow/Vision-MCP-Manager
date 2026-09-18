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

const { checkHealthMock, visionInitMock, visionAddMock } = vi.hoisted(() => ({
  checkHealthMock: vi.fn(async () => ({ healthy: false })),
  visionInitMock: vi.fn(async () => "mocked vision_init"),
  visionAddMock: vi.fn(async () => "mocked vision_add"),
}))

vi.mock("./health", async (importOriginal) => ({
  ...(await importOriginal<typeof import("./health")>()),
  checkHealth: checkHealthMock,
}))

vi.mock("./tools", async (importOriginal) => ({
  ...(await importOriginal<typeof import("./tools")>()),
  visionInit: visionInitMock,
  visionAdd: visionAddMock,
}))

import VisionPlugin from "./index"
import { renderVisionContext } from "./context"
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
  // The dual entry carries the V1 factory on `server`; 1.18.x loads it there.
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  return await (VisionPlugin as any).server(fakePluginInput(directory))
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

// =============================================================================
// Dual entry (V1/V2 loader shape)
// =============================================================================

describe("dual entry loader shape", () => {
  it("default export carries id, setup, and server", () => {
    const entry = VisionPlugin as unknown as Record<string, unknown>

    expect(typeof entry).toBe("object")
    expect(entry).not.toBeNull()
    expect(typeof entry.id, "V2 requires a string plugin id").toBe("string")
    expect(typeof entry.setup, "V2 calls setup(context)").toBe("function")
    expect(typeof entry.server, "1.18.x extracts and invokes the server member").toBe("function")
  })

  it("keeps the module at a single default export the V1 loader can consume", async () => {
    // The 1.18.x loader path that iterates module values throws
    // "Plugin export is not a function" for any value that is neither a
    // function nor an object carrying `server`. A second named export would
    // trip it; the default object carrying `server` does not.
    const module = await import("./index")
    const values = Object.values(module)

    expect(values).toHaveLength(1)
    const exported = values[0] as Record<string, unknown>
    expect(typeof exported.server).toBe("function")
  })
})

// =============================================================================
// V2 setup registration
// =============================================================================

interface RecordedV2Tool {
  name: string
  description: string
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  input: any
  execute: (raw: unknown) => Promise<unknown>
}

interface FakeV2Mcp {
  reload: ReturnType<typeof vi.fn>
  updates: Array<{ name: string; disabled: boolean | undefined }>
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  transform: any
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  list: any
}

/**
 * Build a fake mcp domain for the ctx.mcp control tools. `configs` models the
 * server config entries (absent name = not configured); `statusAfter` is what
 * `list()` reports after reload.
 */
function fakeV2Mcp(
  configs: Record<string, { disabled?: boolean }>,
  statusAfter: string
): FakeV2Mcp {
  const updates: Array<{ name: string; disabled: boolean | undefined }> = []
  const reload = vi.fn(async () => {})
  return {
    reload,
    updates,
    transform: async (callback: (editor: unknown) => void) => {
      callback({
        get: (name: string) => configs[name],
        update: (name: string, apply: (config: { disabled?: boolean }) => void) => {
          const config = { ...configs[name] }
          apply(config)
          updates.push({ name, disabled: config.disabled })
        },
      })
      return { dispose: async () => {} }
    },
    list: async () => ({
      location: { directory: "/tmp/project" },
      data: Object.keys(configs).map((name) => ({ name, status: { status: statusAfter } })),
    }),
  }
}

interface V2SetupFixture {
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  cleanup: any
  added: Record<string, RecordedV2Tool>
  hooks: Record<string, (input: unknown) => Promise<void> | void>
  eventSubscribed: boolean
}

async function runV2Setup(
  codeMode: boolean,
  overrides?: { mcp?: FakeV2Mcp }
): Promise<V2SetupFixture> {
  vi.stubEnv("OPENCODE_EXPERIMENTAL_CODE_MODE", codeMode ? "true" : "false")

  const added: Record<string, RecordedV2Tool> = {}
  const hooks: Record<string, (input: unknown) => Promise<void> | void> = {}
  let eventSubscribed = false

  const editor = {
    add: (tool: RecordedV2Tool) => {
      added[tool.name] = tool
    },
  }

  const context = {
    location: { directory: "/tmp/project" },
    event: {
      subscribe: () => {
        eventSubscribed = true
        return (async function* () {})()
      },
    },
    session: {
      hook: async (name: string, callback: (input: unknown) => Promise<void> | void) => {
        hooks[name] = callback
        return { dispose: async () => {} }
      },
    },
    tool: {
      transform: async (callback: (input: unknown) => void) => {
        callback(editor)
        return { dispose: async () => {} }
      },
    },
    mcp: overrides?.mcp ?? fakeV2Mcp({}, "connected"),
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
  } as any

  const cleanup = await (
    VisionPlugin as unknown as { setup: (context: unknown) => Promise<unknown> }
  ).setup(context)

  return { cleanup, added, hooks, eventSubscribed }
}

describe("V2 setup registration", () => {
  beforeEach(() => {
    checkHealthMock.mockClear()
    visionInitMock.mockClear()
    visionAddMock.mockClear()
  })

  afterEach(() => {
    vi.unstubAllEnvs()
  })

  it("subscribes to the event stream and registers the compaction hook", async () => {
    const fixture = await runV2Setup(false)

    expect(fixture.eventSubscribed).toBe(true)
    expect(Object.keys(fixture.hooks)).toContain("compaction")
  })

  it("registers exactly the declared tool names with JSON Schema inputs (Code Mode OFF)", async () => {
    const fixture = await runV2Setup(false)
    const registered = Object.keys(fixture.added)

    expect(new Set(registered)).toEqual(new Set(expectedNames))
    expect(registered).toHaveLength(expectedNames.length)

    for (const name of registered) {
      const tool = fixture.added[name]
      expect(typeof tool.description, `${name} description`).toBe("string")
      expect(tool.description.length).toBeGreaterThan(0)
      expect(typeof tool.execute, `${name} execute`).toBe("function")
      expect(tool.input, `${name} input must be a JSON Schema object`).toBeDefined()
      expect(tool.input.type).toBe("object")
      expect(tool.input.properties).toBeDefined()
    }
  })

  it("registers only the plugin-local tools under Code Mode ON (V2)", async () => {
    const fixture = await runV2Setup(true)
    const registered = Object.keys(fixture.added)

    expect(new Set(registered)).toEqual(new Set(pluginLocalNames))
    expect(
      registered.filter((name) => daemonNames.includes(name)),
      "daemon-proxy tools are reachable as tools.vision.* under Code Mode"
    ).toEqual([])
  })

  it("derives vision_add JSON Schema from the zod schema", async () => {
    const fixture = await runV2Setup(false)
    const input = fixture.added[VISION_PLUGIN_TOOL_NAMES.add].input

    expect(input.properties.name.type).toBe("string")
    expect(input.properties.start.type).toBe("boolean")
    expect(input.required).toContain("name")
    // `.default(true)` is an optional INPUT under io: "input" — the caller may
    // omit it and the zod parse applies the default.
    expect(input.required ?? []).not.toContain("start")
    expect(input.properties.name.description).toContain("registry")
  })

  it("keeps zod as the internal parse layer, applying schema defaults", async () => {
    const fixture = await runV2Setup(false)
    const tool = fixture.added[VISION_PLUGIN_TOOL_NAMES.add]

    const result = (await tool.execute({ name: "linear" })) as { content: string }

    // Raw input omitted `start`; the zod parse inside execute applied the
    // default before the handler ran.
    expect(visionAddMock).toHaveBeenCalledWith({ name: "linear", start: true })
    expect(result).toEqual({ content: "mocked vision_add" })
  })

  it("injects the Vision context as a text system part on compaction", async () => {
    const fixture = await runV2Setup(false)
    const compaction = fixture.hooks["compaction"]

    const input = { system: [] as Array<{ type: string; text: string }> }
    await compaction?.(input)

    expect(input.system).toHaveLength(1)
    expect(input.system[0].type).toBe("text")
    expect(input.system[0].text).toBe(renderVisionContext({ healthy: false, codeMode: false }))
  })

  it("checks daemon health on session.created events", async () => {
    checkHealthMock.mockClear()

    vi.stubEnv("OPENCODE_EXPERIMENTAL_CODE_MODE", "false")
    const context = {
      location: { directory: "/tmp/project" },
      event: {
        subscribe: () =>
          (async function* () {
            yield { type: "session.created", data: { sessionID: "sess-1" } }
          })(),
      },
      session: {
        hook: async () => ({ dispose: async () => {} }),
      },
      tool: {
        transform: async () => ({ dispose: async () => {} }),
      },
      mcp: fakeV2Mcp({}, "connected"),
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
    } as any

    await (VisionPlugin as unknown as { setup: (context: unknown) => Promise<unknown> }).setup(
      context
    )

    // One startup check + one per session.created event.
    await vi.waitFor(() => expect(checkHealthMock).toHaveBeenCalledTimes(2))
  })

  it("connects a declared disabled server through the ctx.mcp domain", async () => {
    const mcp = fakeV2Mcp({ linear: { disabled: true } }, "connected")
    const fixture = await runV2Setup(false, { mcp })

    const result = JSON.parse(
      (
        (await fixture.added[OPENCODE_MCP_TOOL_NAMES.mcpConnect].execute({
          name: "linear",
        })) as { content: string }
      ).content
    )

    expect(result.success).toBe(true)
    expect(result.status).toBe("connected")
    expect(mcp.updates).toEqual([{ name: "linear", disabled: undefined }])
    expect(mcp.reload).toHaveBeenCalledTimes(1)
  })

  it("disconnects a connected server by setting the disabled flag", async () => {
    const mcp = fakeV2Mcp({ linear: {} }, "disabled")
    const fixture = await runV2Setup(false, { mcp })

    const result = JSON.parse(
      (
        (await fixture.added[OPENCODE_MCP_TOOL_NAMES.mcpDisconnect].execute({
          name: "linear",
        })) as { content: string }
      ).content
    )

    expect(result.success).toBe(true)
    expect(result.status).toBe("disabled")
    expect(mcp.updates).toEqual([{ name: "linear", disabled: true }])
    expect(mcp.reload).toHaveBeenCalledTimes(1)
  })

  it("refuses to connect a server that is not declared in the config", async () => {
    const mcp = fakeV2Mcp({}, "connected")
    const fixture = await runV2Setup(false, { mcp })

    const result = JSON.parse(
      (
        (await fixture.added[OPENCODE_MCP_TOOL_NAMES.mcpConnect].execute({
          name: "ghost",
        })) as { content: string }
      ).content
    )

    expect(result.success).toBe(false)
    expect(result.error).toContain("not configured")
    expect(result.suggestion).toContain('"mcp" block')
    expect(mcp.reload).not.toHaveBeenCalled()
  })

  it("reports a failed attach with the upstream error verbatim", async () => {
    const mcp = fakeV2Mcp(
      { linear: { disabled: true } },
      // The fake list() reports this status for every server after reload.
      "failed"
    )
    // The failed variant carries an error message; patch the fake's list data.
    mcp.list = async () => ({
      location: { directory: "/tmp/project" },
      data: [{ name: "linear", status: { status: "failed", error: "boom" } }],
    })
    const fixture = await runV2Setup(false, { mcp })

    const result = JSON.parse(
      (
        (await fixture.added[OPENCODE_MCP_TOOL_NAMES.mcpConnect].execute({
          name: "linear",
        })) as { content: string }
      ).content
    )

    expect(result.success).toBe(false)
    expect(result.error).toBe("boom")
  })
})
