/**
 * Unit tests for the MCP client session lifecycle.
 *
 * Covers the scenarios that the live integration test cannot exercise
 * deterministically:
 *   - protocolVersion mismatch → structured error
 *   - SSE response parsing (multi-line `data:` frames)
 *   - HTTP 404 → re-init once → retry succeeds
 *   - Daemon pre-dispatch rejection → re-init once → retry succeeds
 *   - Single-flight: N concurrent callTool → exactly 1 initialize POST
 *
 * Mocks `globalThis.fetch` so tests do not hit the network.
 */
import { describe, it, expect, beforeEach, vi, afterEach } from "vitest"

// We import * as a namespace so we can reset module state between tests by
// re-importing via dynamic import. Vitest isolates modules per test file by
// default but cached state inside the module (our session cache) needs an
// explicit reset.

// Build mock fetch helpers
type FetchResponseInit = {
  status?: number
  headers?: Record<string, string>
  body: string | object
}

function makeResponse({ status = 200, headers = {}, body }: FetchResponseInit): Response {
  const text = typeof body === "string" ? body : JSON.stringify(body)
  const responseHeaders = new Headers(headers)
  return new Response(text, { status, headers: responseHeaders })
}

// JSON-RPC initialize response payload
function initResponse(
  sessionId: string,
  protocolVersion = "2025-03-26",
  asSse = false
): FetchResponseInit {
  const payload = {
    jsonrpc: "2.0",
    id: 1,
    result: {
      protocolVersion,
      capabilities: {},
      serverInfo: { name: "vision", version: "test" },
    },
  }
  if (asSse) {
    return {
      status: 200,
      headers: {
        "content-type": "text/event-stream",
        "mcp-session-id": sessionId,
      },
      body: `event: message\ndata: ${JSON.stringify(payload)}\n\n`,
    }
  }
  return {
    status: 200,
    headers: {
      "content-type": "application/json",
      "mcp-session-id": sessionId,
    },
    body: payload,
  }
}

// JSON-RPC tools/call response payload
function toolCallResponse(text: string, id = 2, asSse = false): FetchResponseInit {
  const payload = {
    jsonrpc: "2.0",
    id,
    result: {
      content: [{ type: "text", text }],
    },
  }
  if (asSse) {
    return {
      status: 200,
      headers: { "content-type": "text/event-stream" },
      body: `event: message\ndata: ${JSON.stringify(payload)}\n\n`,
    }
  }
  return {
    status: 200,
    headers: { "content-type": "application/json" },
    body: payload,
  }
}

// Helper to count how many initialize POSTs the mock saw
function countInitializeCalls(fetchMock: ReturnType<typeof vi.fn>): number {
  return fetchMock.mock.calls.filter((call) => {
    const init = call[1] as RequestInit
    if (!init || !init.body) return false
    try {
      const body = JSON.parse(init.body as string)
      return body.method === "initialize"
    } catch {
      return false
    }
  }).length
}

describe("MCP client session unit lifecycle", () => {
  let fetchMock: ReturnType<typeof vi.fn>

  beforeEach(async () => {
    fetchMock = vi.fn()
    globalThis.fetch = fetchMock as unknown as typeof fetch
    // Reset module state by clearing the require cache equivalent:
    // vi.resetModules() + dynamic import per test.
    vi.resetModules()
  })

  afterEach(() => {
    vi.restoreAllMocks()
  })

  async function loadClient() {
    // Dynamic import after resetModules gives fresh module state.
    return await import("./mcp-client")
  }

  it("protocolVersion mismatch throws structured error (no retry)", async () => {
    fetchMock.mockImplementation(async (_url: string, init: RequestInit) => {
      const body = JSON.parse(init.body as string) as { method: string }
      if (body.method === "initialize") {
        return makeResponse(initResponse("sid-mismatch", "2024-11-05"))
      }
      return makeResponse({ status: 500, body: "unexpected" })
    })

    const { callTool } = await loadClient()
    const result = await callTool("vision_status", {})
    const parsed = JSON.parse(result)
    expect(parsed.success).toBe(false)
    expect(parsed.error).toMatch(/protocolVersion mismatch/)
  })

  it("parses SSE responses (text/event-stream with data: frame)", async () => {
    fetchMock.mockImplementation(async (_url: string, init: RequestInit) => {
      const body = JSON.parse(init.body as string) as { method: string }
      if (body.method === "initialize") {
        return makeResponse(initResponse("sid-sse", undefined, true))
      }
      if (body.method === "notifications/initialized") {
        return makeResponse({ status: 202, body: "" })
      }
      // tools/call returns SSE
      return makeResponse(toolCallResponse('{"healthy":true}', 2, true))
    })

    const { callTool } = await loadClient()
    const result = await callTool("vision_status", {})
    const parsed = JSON.parse(result)
    expect(parsed.healthy).toBe(true)
  })

  it("HTTP 404 on tools/call triggers one re-init + retry, then succeeds", async () => {
    let toolCallAttempt = 0
    fetchMock.mockImplementation(async (_url: string, init: RequestInit) => {
      const body = JSON.parse(init.body as string) as { method: string }
      if (body.method === "initialize") {
        return makeResponse(initResponse("sid-404-retry"))
      }
      if (body.method === "notifications/initialized") {
        return makeResponse({ status: 202, body: "" })
      }
      // tools/call: first attempt → 404, second attempt → success
      toolCallAttempt += 1
      if (toolCallAttempt === 1) {
        return makeResponse({ status: 404, body: "session expired" })
      }
      return makeResponse(toolCallResponse('{"healthy":true}'))
    })

    const { callTool } = await loadClient()
    const result = await callTool("vision_status", {})
    const parsed = JSON.parse(result)
    expect(parsed.healthy).toBe(true)

    // Exactly 2 initialize POSTs (initial + retry after 404)
    expect(countInitializeCalls(fetchMock)).toBe(2)
    // Exactly 2 tools/call POSTs (failed + succeeded)
    const toolCallCount = fetchMock.mock.calls.filter((call) => {
      const init = call[1] as RequestInit
      if (!init || !init.body) return false
      const body = JSON.parse(init.body as string)
      return body.method === "tools/call"
    }).length
    expect(toolCallCount).toBe(2)
  })

  it("daemon pre-dispatch rejection triggers one re-init + retry", async () => {
    let toolCallAttempt = 0
    fetchMock.mockImplementation(async (_url: string, init: RequestInit) => {
      const body = JSON.parse(init.body as string) as { method: string }
      if (body.method === "initialize") {
        return makeResponse(initResponse("sid-rejection-retry"))
      }
      if (body.method === "notifications/initialized") {
        return makeResponse({ status: 202, body: "" })
      }
      toolCallAttempt += 1
      if (toolCallAttempt === 1) {
        return makeResponse({
          body: {
            jsonrpc: "2.0",
            id: 2,
            error: {
              code: 0,
              message: 'method "tools/call" is invalid during session initialization',
            },
          },
        })
      }
      return makeResponse(toolCallResponse('{"healthy":true}'))
    })

    const { callTool } = await loadClient()
    const result = await callTool("vision_status", {})
    const parsed = JSON.parse(result)
    expect(parsed.healthy).toBe(true)
    expect(countInitializeCalls(fetchMock)).toBe(2)
  })

  it("single-flight: 5 concurrent callTool → exactly 1 initialize POST", async () => {
    fetchMock.mockImplementation(async (_url: string, init: RequestInit) => {
      const body = JSON.parse(init.body as string) as { method: string }
      if (body.method === "initialize") {
        // Yield to allow concurrent callers to queue on the in-flight promise.
        await new Promise((r) => setTimeout(r, 10))
        return makeResponse(initResponse("sid-single-flight"))
      }
      if (body.method === "notifications/initialized") {
        return makeResponse({ status: 202, body: "" })
      }
      return makeResponse(toolCallResponse('{"healthy":true}'))
    })

    const { callTool } = await loadClient()
    const results = await Promise.all(
      Array.from({ length: 5 }, () => callTool("vision_status", {}))
    )
    for (const r of results) {
      expect(JSON.parse(r).healthy).toBe(true)
    }
    expect(countInitializeCalls(fetchMock)).toBe(1)
  })

  it("missing Mcp-Session-Id header on initialize → structured error", async () => {
    fetchMock.mockImplementation(async (_url: string, init: RequestInit) => {
      const body = JSON.parse(init.body as string) as { method: string }
      if (body.method === "initialize") {
        // Intentionally no mcp-session-id header
        return makeResponse({
          status: 200,
          headers: { "content-type": "application/json" },
          body: {
            jsonrpc: "2.0",
            id: 1,
            result: { protocolVersion: "2025-03-26" },
          },
        })
      }
      return makeResponse({ status: 500, body: "unexpected" })
    })

    const { callTool } = await loadClient()
    const result = await callTool("vision_status", {})
    const parsed = JSON.parse(result)
    expect(parsed.success).toBe(false)
    expect(parsed.error).toMatch(/Mcp-Session-Id/)
  })
})
