import type { McpStatus } from "@opencode-ai/sdk"
import type { PluginInput } from "@opencode-ai/plugin"
import type { Plugin } from "@opencode/plugin"
import { z } from "zod"

export const McpConnectArgsSchema = z.object({
  name: z
    .string()
    .describe('Name of the MCP server to connect, as declared in the opencode config "mcp" block'),
})

export const McpDisconnectArgsSchema = z.object({
  name: z.string().describe("Name of the MCP server to disconnect"),
})

type McpControlClient = Pick<PluginInput["client"], "mcp">

const notConfiguredSuggestion =
  'Add it to the "mcp" block of opencode.jsonc, then restart OpenCode. opencode_mcp_connect only enables servers that are already declared.'

function notConfigured(name: string): string {
  return JSON.stringify({
    success: false,
    server: name,
    error: `MCP server "${name}" is not configured in this project.`,
    suggestion: notConfiguredSuggestion,
  })
}

function unexpectedError(name: string, error: unknown): string {
  return JSON.stringify({
    success: false,
    server: name,
    error: error instanceof Error ? error.message : "Unknown error controlling MCP server",
  })
}

// Exhaustive over the McpStatus union. The `never` default makes a new upstream
// variant a compile error rather than a silent fall-through (DDC7). Where the
// upstream status carries an `error`, it is surfaced verbatim (DDC4) and our own
// guidance goes in `suggestion` — the slot the rest of the plugin already uses.
function statusFailure(name: string, status: McpStatus): string {
  switch (status.status) {
    case "connected":
      return JSON.stringify({
        success: false,
        server: name,
        status: status.status,
        error: `MCP server "${name}" did not reach the requested state (status: connected).`,
      })
    case "disabled":
      return JSON.stringify({
        success: false,
        server: name,
        status: status.status,
        error: `MCP server "${name}" did not attach after connect (status: disabled).`,
      })
    case "failed":
      return JSON.stringify({
        success: false,
        server: name,
        status: status.status,
        error: status.error,
      })
    case "needs_auth":
      return JSON.stringify({
        success: false,
        server: name,
        status: status.status,
        error: `MCP server "${name}" needs authentication before it can connect.`,
        suggestion: `Run: opencode mcp auth ${name}`,
      })
    case "needs_client_registration":
      return JSON.stringify({
        success: false,
        server: name,
        status: status.status,
        error: status.error,
        suggestion: `OAuth client credentials are missing from the "${name}" entry in the opencode config "mcp" block. Add oauth.clientId (and clientSecret if the provider requires it).`,
      })
    default: {
      const exhaustiveStatus: never = status
      return exhaustiveStatus
    }
  }
}

function missingStatus(name: string, operation: "connect" | "disconnect"): string {
  return JSON.stringify({
    success: false,
    server: name,
    error: `MCP server "${name}" vanished from the registry after ${operation}.`,
  })
}

export async function connectMcpServer(
  client: McpControlClient,
  args: z.infer<typeof McpConnectArgsSchema>
): Promise<string> {
  try {
    const result = await client.mcp.connect({ path: { name: args.name } })
    if (result.response.status === 404) return notConfigured(args.name)

    // Exactly one status read (DDC3). opencode commits the final status before
    // the connect call resolves, so there is nothing to wait for.
    const status = (await client.mcp.status()).data?.[args.name]
    if (!status) return missingStatus(args.name, "connect")

    if (status.status === "connected") {
      return JSON.stringify({
        success: true,
        server: args.name,
        status: status.status,
        message: `MCP server "${args.name}" connected. Its tools become available on your next turn.`,
      })
    }
    return statusFailure(args.name, status)
  } catch (error) {
    return unexpectedError(args.name, error)
  }
}

export async function disconnectMcpServer(
  client: McpControlClient,
  args: z.infer<typeof McpDisconnectArgsSchema>
): Promise<string> {
  try {
    const result = await client.mcp.disconnect({ path: { name: args.name } })
    if (result.response.status === 404) return notConfigured(args.name)

    const status = (await client.mcp.status()).data?.[args.name]
    if (!status) return missingStatus(args.name, "disconnect")

    if (status.status === "disabled") {
      return JSON.stringify({
        success: true,
        server: args.name,
        status: status.status,
        message: `MCP server "${args.name}" disconnected. Its tools are gone from your next turn. Its config entry is unchanged, so it can be reconnected.`,
      })
    }
    return statusFailure(args.name, status)
  } catch (error) {
    return unexpectedError(args.name, error)
  }
}

// =============================================================================
// V2 (ctx.mcp domain)
// =============================================================================
//
// The V2 plugin context is itself the client. Its `mcp` domain carries a
// config editor plus `reload()` and `list()`, not the V1 connect/disconnect
// RPCs — so connect means "clear the server config's `disabled` flag and
// reload", and disconnect means "set `disabled` and reload". The config entry
// stays untouched otherwise, matching the V1 contract that a disconnected
// server can be reconnected.

type V2McpDomain = Plugin.Context["mcp"]

type V2McpStatus = Awaited<ReturnType<V2McpDomain["list"]>>["data"][number]["status"]

// Exhaustive over the V2 status union. The `never` default makes a new
// upstream variant a compile error rather than a silent fall-through (DDC7),
// mirroring the V1 statusFailure above.
function statusFailureV2(name: string, status: V2McpStatus): string {
  switch (status.status) {
    case "connected":
      return JSON.stringify({
        success: false,
        server: name,
        status: status.status,
        error: `MCP server "${name}" did not reach the requested state (status: connected).`,
      })
    case "pending":
      return JSON.stringify({
        success: false,
        server: name,
        status: status.status,
        error: `MCP server "${name}" is still starting (status: pending).`,
      })
    case "disabled":
      return JSON.stringify({
        success: false,
        server: name,
        status: status.status,
        error: `MCP server "${name}" did not attach after connect (status: disabled).`,
      })
    case "failed":
      return JSON.stringify({
        success: false,
        server: name,
        status: status.status,
        error: status.error,
      })
    case "needs_auth":
      return JSON.stringify({
        success: false,
        server: name,
        status: status.status,
        error: `MCP server "${name}" needs authentication before it can connect.`,
        suggestion: `Run: opencode mcp auth ${name}`,
      })
    default: {
      const exhaustiveStatus: never = status
      return exhaustiveStatus
    }
  }
}

async function findStatusV2(mcp: V2McpDomain, name: string): Promise<V2McpStatus | undefined> {
  const server = (await mcp.list()).data.find((entry) => entry.name === name)
  return server?.status
}

export async function connectMcpServerV2(
  mcp: V2McpDomain,
  args: z.infer<typeof McpConnectArgsSchema>
): Promise<string> {
  try {
    let configured = false
    await mcp.transform((editor) => {
      if (!editor.get(args.name)) return
      configured = true
      editor.update(args.name, (config) => {
        delete config.disabled
      })
    })
    if (!configured) return notConfigured(args.name)

    await mcp.reload()

    const status = await findStatusV2(mcp, args.name)
    if (!status) return missingStatus(args.name, "connect")

    if (status.status === "connected") {
      return JSON.stringify({
        success: true,
        server: args.name,
        status: status.status,
        message: `MCP server "${args.name}" connected. Its tools become available on your next turn.`,
      })
    }
    return statusFailureV2(args.name, status)
  } catch (error) {
    return unexpectedError(args.name, error)
  }
}

export async function disconnectMcpServerV2(
  mcp: V2McpDomain,
  args: z.infer<typeof McpDisconnectArgsSchema>
): Promise<string> {
  try {
    let configured = false
    await mcp.transform((editor) => {
      if (!editor.get(args.name)) return
      configured = true
      editor.update(args.name, (config) => {
        config.disabled = true
      })
    })
    if (!configured) return notConfigured(args.name)

    await mcp.reload()

    const status = await findStatusV2(mcp, args.name)
    if (!status) return missingStatus(args.name, "disconnect")

    if (status.status === "disabled") {
      return JSON.stringify({
        success: true,
        server: args.name,
        status: status.status,
        message: `MCP server "${args.name}" disconnected. Its tools are gone from your next turn. Its config entry is unchanged, so it can be reconnected.`,
      })
    }
    return statusFailureV2(args.name, status)
  } catch (error) {
    return unexpectedError(args.name, error)
  }
}
