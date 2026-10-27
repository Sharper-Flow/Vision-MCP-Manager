import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"
import type { Plugin as V2Plugin } from "@opencode/plugin"

vi.mock("./health", () => ({ isDaemonRunning: async () => true }))

const status = {
  healthy: false,
  uptime: "1m",
  servers: { running: 0, stopped: 1, error: 0 },
  memory_mb: 12.5,
  warnings: ["probe pending"],
}
const failure = {
  success: false,
  error: "Server added but failed to start",
  code: "START_FAILED",
  suggestion: "Inspect the server logs before restarting",
  name: "example",
  details: { phase: "start", retryable: false },
}
const text = (payload: unknown) => [{ type: "text", text: JSON.stringify(payload) }]

describe("validated Vision proxy results", () => {
  let toolCalls: number
  let result: unknown
  let rpc: unknown
  let transport: (() => Promise<Response>) | undefined

  beforeEach(() => {
    vi.resetModules()
    toolCalls = 0
    result = { structuredContent: status }
    rpc = undefined
    transport = undefined
    vi.stubEnv("OPENCODE_EXPERIMENTAL_CODE_MODE", "false")
    vi.stubGlobal(
      "fetch",
      vi.fn(async (_url: string, init: RequestInit) => {
        const request = JSON.parse(init.body as string)
        if (request.method === "initialize") {
          return Response.json(
            { jsonrpc: "2.0", id: request.id, result: { protocolVersion: "2025-03-26" } },
            { headers: { "mcp-session-id": "fixture-session" } }
          )
        }
        if (request.method === "notifications/initialized") {
          return new Response(null, { status: 202 })
        }
        toolCalls++
        if (transport) return transport()
        return Response.json(rpc ?? { jsonrpc: "2.0", id: request.id, result })
      })
    )
  })

  afterEach(() => {
    vi.unstubAllGlobals()
    vi.unstubAllEnvs()
  })

  async function call(name = "vision_status") {
    const { callTool } = await import("./mcp-client")
    return JSON.parse(await callTool(name, { name: "example" }))
  }

  it("preserves structured-only data and distinguishes unhealthy from a failed call", async () => {
    expect(await call()).toEqual({ ...status, success: true })
  })

  it("intentionally parses text-only JSON and preserves payload fields", async () => {
    result = { content: text(status) }
    expect(await call()).toEqual({ ...status, success: true })
  })

  it("joins split legacy text blocks before parsing JSON", async () => {
    const json = JSON.stringify(status)
    result = {
      content: [
        { type: "text", text: json.slice(0, 1) },
        { type: "text", text: json.slice(1) },
      ],
    }
    expect(await call()).toEqual({ ...status, success: true })
  })

  it("prefers structured data over conflicting or non-JSON presentation text", async () => {
    result = { structuredContent: status, content: [{ type: "text", text: "All healthy!" }] }
    expect(await call()).toEqual({ ...status, success: true })
  })

  it("preserves unknown structured extension fields", async () => {
    result = { structuredContent: { ...status, extension: { values: [null, 0, false, ""] } } }
    expect((await call()).extension).toEqual({ values: [null, 0, false, ""] })
  })

  it.each([
    ["missing content", {}],
    ["empty content", { content: [] }],
    ["non-text content only", { content: [{ type: "image", data: "abc", mimeType: "image/png" }] }],
    ["empty text", { content: [{ type: "text", text: "" }] }],
    ["malformed JSON", { content: [{ type: "text", text: "{broken" }] }],
    ["plain success text", { content: [{ type: "text", text: "done" }] }],
    ["JSON null", { content: text(null) }],
    ["JSON array", { content: text([]) }],
    ["JSON scalar", { content: text(42) }],
    ["empty object", { content: text({}) }],
    ["wrong daemon shape", { structuredContent: { healthy: "yes" } }],
    ["missing daemon fields", { structuredContent: { healthy: true } }],
    [
      "wrong nested field",
      { structuredContent: { ...status, servers: { running: "one", stopped: 0, error: 0 } } },
    ],
    [
      "invalid structured data with valid text",
      { structuredContent: null, content: text({ success: true, name: "example" }) },
    ],
    [
      "invalid structured shape with valid text",
      { structuredContent: {}, content: text({ success: true, name: "example" }) },
    ],
    ["malformed content", { content: "wrong" }],
    ["malformed text block", { content: [{ type: "text", text: 42 }] }],
    ["malformed isError", { structuredContent: status, isError: "false" }],
    ["missing error identity", { structuredContent: { success: false } }],
    ["malformed recovery hint", { structuredContent: { ...failure, suggestion: 42 } }],
    ["null result", null],
  ])("rejects %s without replaying a potentially completed mutation", async (_label, fixture) => {
    result = fixture
    const outcome = await call("vision_add")
    expect(outcome).toMatchObject({ success: false, kind: "invalid_result" })
    expect(outcome.error).toMatch(/Invalid Vision/)
    expect(outcome.suggestion).toContain("Do not automatically repeat")
    expect(toolCalls).toBe(1)
  })

  it("preserves structured daemon failure identity and recovery hints", async () => {
    result = { structuredContent: failure }
    expect(await call("vision_add")).toEqual(failure)
  })

  it("preserves text-only daemon failure identity and recovery hints", async () => {
    result = { content: text(failure) }
    expect(await call("vision_add")).toEqual(failure)
  })

  it("honors MCP isError even if the payload claims success", async () => {
    result = {
      isError: true,
      structuredContent: { ...failure, success: true },
      content: text(status),
    }
    expect(await call("vision_add")).toEqual({ ...failure, isError: true, kind: "mcp_tool" })
    expect(toolCalls).toBe(1)
  })

  it("turns plain MCP error text into a declared failure", async () => {
    result = { isError: true, content: [{ type: "text", text: "catalog not initialized" }] }
    expect(await call()).toMatchObject({
      success: false,
      error: "catalog not initialized",
      isError: true,
      kind: "mcp_tool",
    })
  })

  it("preserves a daemon-owned error kind rather than replacing its identity", async () => {
    result = { isError: true, structuredContent: { ...failure, kind: "start_failed" } }
    expect(await call("vision_add")).toEqual({ ...failure, kind: "start_failed", isError: true })
  })

  it("keeps MCP error semantics when there is no message", async () => {
    result = { isError: true, content: [] }
    expect(await call()).toMatchObject({ success: false, isError: true, kind: "mcp_tool" })
  })

  it("keeps MCP error semantics even when structured error data is malformed", async () => {
    result = { isError: true, structuredContent: null, content: text(failure) }
    expect(await call()).toMatchObject({ success: false, isError: true, kind: "invalid_result" })
    expect(toolCalls).toBe(1)
  })

  it("preserves JSON-RPC code and arbitrary data, including recovery information", async () => {
    const data = { suggestion: "Check the argument name", reason: { field: "name" } }
    rpc = { jsonrpc: "2.0", id: 2, error: { code: -32602, message: "invalid params", data } }
    expect(await call()).toEqual({
      success: false,
      error: "invalid params",
      code: -32602,
      data,
      kind: "json_rpc",
    })
    expect(toolCalls).toBe(1)
  })

  it.each([
    null,
    [],
    {},
    { jsonrpc: "1.0", id: 2, result: {} },
    { jsonrpc: "2.0", id: 2, error: { message: "no code" } },
    { jsonrpc: "2.0", id: 2, result: {}, error: { code: 0, message: "ambiguous" } },
  ])("rejects malformed RPC envelope %#", async (fixture) => {
    transport = async () => Response.json(fixture)
    expect(await call()).toMatchObject({ success: false, kind: "invalid_result" })
    expect(toolCalls).toBe(1)
  })

  it("rejects malformed wire JSON without replay", async () => {
    transport = async () =>
      new Response("{broken", { headers: { "content-type": "application/json" } })
    expect(await call("vision_add")).toMatchObject({ success: false, kind: "invalid_result" })
    expect(toolCalls).toBe(1)
  })

  it("consumes structured results over SSE", async () => {
    transport = async () =>
      new Response(
        `event: message\ndata: ${JSON.stringify({ jsonrpc: "2.0", id: 2, result: { structuredContent: status } })}\n\n`,
        { headers: { "content-type": "text/event-stream" } }
      )
    expect(await call()).toEqual({ ...status, success: true })
  })

  it("preserves timeout identity", async () => {
    transport = async () => {
      throw new DOMException("aborted", "AbortError")
    }
    expect(await call()).toMatchObject({
      success: false,
      kind: "timeout",
      error: expect.stringContaining("request timeout"),
    })
    expect(toolCalls).toBe(1)
  })

  it("preserves body timeout identity", async () => {
    vi.stubEnv("VISION_BODY_TIMEOUT_MS", "1")
    transport = async () =>
      ({
        ok: true,
        status: 200,
        headers: new Headers(),
        json: () => new Promise(() => {}),
      }) as Response
    expect(await call()).toMatchObject({
      success: false,
      kind: "timeout",
      error: "Response body read timeout",
    })
    expect(toolCalls).toBe(1)
  })

  it("preserves connection failure recovery guidance", async () => {
    transport = async () => {
      throw new TypeError("fetch failed", { cause: { code: "ECONNREFUSED" } })
    }
    expect(await call()).toMatchObject({
      success: false,
      kind: "connection",
      error: "Vision daemon is not running",
      data: { message: "fetch failed", code: "ECONNREFUSED" },
      suggestion: "Restart it with: systemctl --user restart vision.service",
    })
    expect(toolCalls).toBe(1)
  })

  it.each([
    ["vision_list", { servers: [], slot_groups: [] }],
    ["vision_add", { success: true, name: "example", status: "running" }],
    ["vision_remove", { success: true, name: "example" }],
    ["vision_restart", { success: true, name: "example", status: "stopped" }],
    ["vision_search", { success: true, results: [] }],
    ["vision_init", { success: true, path: "/tmp/opencode.jsonc", servers: ["example"] }],
    ["vision_status", status],
    ["vision_guidance", { success: true, global_guidance: "Use context7" }],
    ["vision_slot_status", { groups: [] }],
    ["vision_slot_status", { groups: [{ group_name: "empty", slot_count: 0, slots: null }] }],
    [
      "vision_metrics",
      { sessions_active: 0, tool_calls_total: 1, errors_total: 0, subprocesses_active: 0 },
    ],
  ])("accepts the daemon-owned %s payload", async (name, payload) => {
    result = { structuredContent: payload }
    expect(await call(name as string)).toEqual({ ...(payload as object), success: true })
  })

  it.each([
    ["vision_list", { servers: "wrong", slot_groups: [] }],
    ["vision_add", { success: true, name: 42 }],
    ["vision_remove", { success: true }],
    ["vision_restart", { success: true, name: "example", status: [] }],
    ["vision_search", { success: true, results: {} }],
    ["vision_init", { success: true, path: "/tmp/opencode.jsonc", servers: "example" }],
    ["vision_status", { ...status, servers: { running: "one", stopped: 0, error: 0 } }],
    ["vision_guidance", { success: true, tools: "wrong" }],
    ["vision_slot_status", { groups: [{ group_name: "example", slot_count: 1, slots: "wrong" }] }],
    ["vision_metrics", { sessions_active: 0 }],
  ])("rejects malformed daemon-owned %s payload fields", async (name, payload) => {
    result = { structuredContent: payload }
    expect(await call(name as string)).toMatchObject({ success: false, kind: "invalid_result" })
    expect(toolCalls).toBe(1)
  })

  it.each(["v1", "v2"])(
    "preserves validated results through the %s registration path",
    async (version) => {
      const { default: plugin } = await import("./index")
      if (version === "v1") {
        const hooks = await plugin.server({ client: {}, directory: "/tmp/project" } as Parameters<
          typeof plugin.server
        >[0])
        const registered = hooks.tool!.vision_status
        expect(registered).not.toHaveProperty("outputSchema")
        const returned = await registered.execute(
          {},
          {} as Parameters<typeof registered.execute>[1]
        )
        expect(typeof returned).toBe("string")
        expect(JSON.parse(returned as string)).toEqual({ ...status, success: true })
        result = { content: text(status) }
        expect(
          JSON.parse(
            (await registered.execute({}, {} as Parameters<typeof registered.execute>[1])) as string
          )
        ).toEqual({ ...status, success: true })
        result = { isError: true, structuredContent: failure }
        expect(
          JSON.parse(
            (await registered.execute({}, {} as Parameters<typeof registered.execute>[1])) as string
          )
        ).toEqual({ ...failure, isError: true, kind: "mcp_tool" })
      } else {
        const added: Record<
          string,
          {
            execute: (raw: unknown) => Promise<unknown>
            output?: { safeParse: (value: unknown) => { success: boolean } }
          }
        > = {}
        await plugin.setup({
          location: { directory: "/tmp/project" },
          tool: {
            transform: async (callback: (editor: unknown) => void) => {
              callback({
                add: (tool: { name: string } & (typeof added)[string]) => {
                  added[tool.name] = tool
                },
              })
              return { dispose: async () => {} }
            },
          },
        } as unknown as V2Plugin.Context)
        const registered = added.vision_status
        expect(registered).not.toHaveProperty("outputSchema")
        const returned = (await registered.execute({})) as { content: string; output: unknown }
        expect(JSON.parse(returned.content)).toEqual({ ...status, success: true })
        expect(returned.output).toEqual({ ...status, success: true })
        expect(registered.output?.safeParse(returned.output).success).toBe(true)
        expect(registered.output?.safeParse({ success: false }).success).toBe(false)
        result = { content: text(status) }
        const legacy = (await registered.execute({})) as { content: string; output: unknown }
        expect(legacy.output).toEqual({ ...status, success: true })
        expect(JSON.parse(legacy.content)).toEqual(legacy.output)
        result = { isError: true, structuredContent: failure }
        const failed = (await registered.execute({})) as { content: string; output: unknown }
        expect(failed.output).toEqual({ ...failure, isError: true, kind: "mcp_tool" })
        expect(JSON.parse(failed.content)).toEqual(failed.output)
      }
    }
  )
})
