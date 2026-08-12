import type { PluginInput } from "@opencode-ai/plugin"
import { afterEach, describe, expect, it, vi } from "vitest"
import { connectMcpServer, disconnectMcpServer } from "./mcp-control"

type McpStatusMap = Record<string, { status: string; error?: string }>

function makeClient(
  options: {
    connect?: () => Promise<unknown>
    disconnect?: () => Promise<unknown>
    status?: () => Promise<unknown>
  } = {}
): Pick<PluginInput["client"], "mcp"> {
  const mcp = {
    connect: vi.fn(options.connect ?? (() => Promise.resolve({ response: { status: 200 } }))),
    disconnect: vi.fn(options.disconnect ?? (() => Promise.resolve({ response: { status: 200 } }))),
    status: vi.fn(options.status ?? (() => Promise.resolve({ data: {} }))),
  }

  return { mcp } as unknown as Pick<PluginInput["client"], "mcp">
}

function statusResult(status: McpStatusMap): Promise<{ data: McpStatusMap }> {
  return Promise.resolve({ data: status })
}

afterEach(() => {
  vi.restoreAllMocks()
})

describe("connectMcpServer", () => {
  it("returns success when the server is connected", async () => {
    const client = makeClient({
      status: () => statusResult({ sentry: { status: "connected" } }),
    })

    await expect(connectMcpServer(client, { name: "sentry" })).resolves.toBe(
      JSON.stringify({
        success: true,
        server: "sentry",
        status: "connected",
        message: 'MCP server "sentry" connected. Its tools become available on your next turn.',
      })
    )
    expect(client.mcp.status).toHaveBeenCalledTimes(1)
  })

  it("surfaces a failed status error verbatim", async () => {
    const client = makeClient({
      status: () => statusResult({ sentry: { status: "failed", error: "handshake exploded" } }),
    })

    await expect(connectMcpServer(client, { name: "sentry" })).resolves.toBe(
      JSON.stringify({
        success: false,
        server: "sentry",
        status: "failed",
        error: "handshake exploded",
      })
    )
  })

  it("gives needs_auth a distinct remedy without leaking undefined", async () => {
    const client = makeClient({
      status: () => statusResult({ sentry: { status: "needs_auth" } }),
    })

    const result = await connectMcpServer(client, { name: "sentry" })
    expect(result).not.toContain("undefined")
    expect(JSON.parse(result)).toEqual({
      success: false,
      server: "sentry",
      status: "needs_auth",
      error: 'MCP server "sentry" needs authentication before it can connect.',
      suggestion: "Run: opencode mcp auth sentry",
    })
  })

  it("reports missing OAuth client credentials distinctly", async () => {
    const client = makeClient({
      status: () =>
        statusResult({
          sentry: { status: "needs_client_registration", error: "registration unavailable" },
        }),
    })

    const result = await connectMcpServer(client, { name: "sentry" })
    const parsed = JSON.parse(result)

    // The upstream error is surfaced verbatim (DDC4) rather than concatenated into prose.
    expect(parsed.error).toBe("registration unavailable")
    expect(parsed).toEqual({
      success: false,
      server: "sentry",
      status: "needs_client_registration",
      error: "registration unavailable",
      suggestion:
        'OAuth client credentials are missing from the "sentry" entry in the opencode config "mcp" block. Add oauth.clientId (and clientSecret if the provider requires it).',
    })
    // needs_client_registration must stay distinguishable from a transport failure.
    expect(parsed.status).not.toBe("failed")
  })

  it("does not treat disabled as a successful connection", async () => {
    const client = makeClient({
      status: () => statusResult({ sentry: { status: "disabled" } }),
    })

    await expect(connectMcpServer(client, { name: "sentry" })).resolves.toBe(
      JSON.stringify({
        success: false,
        server: "sentry",
        status: "disabled",
        error: 'MCP server "sentry" did not attach after connect (status: disabled).',
      })
    )
  })

  it("fails when the server is absent from the status map", async () => {
    const client = makeClient({ status: () => statusResult({}) })

    await expect(connectMcpServer(client, { name: "sentry" })).resolves.toBe(
      JSON.stringify({
        success: false,
        server: "sentry",
        error: 'MCP server "sentry" vanished from the registry after connect.',
      })
    )
  })

  it("returns the exact not-configured envelope for a 404", async () => {
    const client = makeClient({
      connect: () => Promise.resolve({ response: { status: 404 } }),
    })

    await expect(connectMcpServer(client, { name: "sentry" })).resolves.toBe(
      JSON.stringify({
        success: false,
        server: "sentry",
        error: 'MCP server "sentry" is not configured in this project.',
        suggestion:
          'Add it to the "mcp" block of opencode.json / .opencode.json, then reload. opencode_mcp_connect only enables servers that are already declared.',
      })
    )
    expect(client.mcp.status).not.toHaveBeenCalled()
  })

  it("returns a structured failure when the SDK throws", async () => {
    const client = makeClient({
      connect: () => Promise.reject(new Error("SDK unavailable")),
    })

    await expect(connectMcpServer(client, { name: "sentry" })).resolves.toBe(
      JSON.stringify({ success: false, server: "sentry", error: "SDK unavailable" })
    )
  })
})

describe("disconnectMcpServer", () => {
  it("returns success when the server is disabled", async () => {
    const client = makeClient({
      status: () => statusResult({ sentry: { status: "disabled" } }),
    })

    await expect(disconnectMcpServer(client, { name: "sentry" })).resolves.toBe(
      JSON.stringify({
        success: true,
        server: "sentry",
        status: "disabled",
        message:
          'MCP server "sentry" disconnected. Its tools are gone from your next turn. Its config entry is unchanged, so it can be reconnected.',
      })
    )
    expect(client.mcp.status).toHaveBeenCalledTimes(1)
  })

  it("returns the not-configured envelope for a disconnect 404", async () => {
    const client = makeClient({
      disconnect: () => Promise.resolve({ response: { status: 404 } }),
    })

    await expect(disconnectMcpServer(client, { name: "sentry" })).resolves.toBe(
      JSON.stringify({
        success: false,
        server: "sentry",
        error: 'MCP server "sentry" is not configured in this project.',
        suggestion:
          'Add it to the "mcp" block of opencode.json / .opencode.json, then reload. opencode_mcp_connect only enables servers that are already declared.',
      })
    )
    expect(client.mcp.status).not.toHaveBeenCalled()
  })

  it("reads status exactly once for each operation", async () => {
    const connectClient = makeClient({
      status: () => statusResult({ sentry: { status: "connected" } }),
    })
    const disconnectClient = makeClient({
      status: () => statusResult({ sentry: { status: "disabled" } }),
    })

    await connectMcpServer(connectClient, { name: "sentry" })
    await disconnectMcpServer(disconnectClient, { name: "sentry" })

    expect(connectClient.mcp.status).toHaveBeenCalledTimes(1)
    expect(disconnectClient.mcp.status).toHaveBeenCalledTimes(1)
  })
})
