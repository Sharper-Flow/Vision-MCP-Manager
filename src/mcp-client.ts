/**
 * MCP Client for Vision Admin Server
 *
 * Provides a simple HTTP client for calling MCP tools on the Vision Admin server.
 * Uses JSON-RPC 2.0 protocol over HTTP POST to /mcp endpoint.
 */

import { getVisionBodyReadTimeoutMs, getVisionRequestTimeoutMs } from "./timeouts"

const ADMIN_PORT = 6275
const MCP_ENDPOINT = `http://localhost:${ADMIN_PORT}/mcp`

/**
 * Read response JSON with timeout protection.
 */
async function readJsonWithTimeout<T>(response: Response, timeoutMs: number): Promise<T> {
  return Promise.race([
    response.json() as Promise<T>,
    new Promise<T>((_, reject) =>
      setTimeout(() => reject(new Error("Response body read timeout")), timeoutMs)
    ),
  ])
}

// JSON-RPC 2.0 types
interface JsonRpcRequest {
  jsonrpc: "2.0"
  method: string
  params?: unknown
  id: number | string
}

interface JsonRpcResponse {
  jsonrpc: "2.0"
  result?: unknown
  error?: {
    code: number
    message: string
    data?: unknown
  }
  id: number | string | null
}

// MCP tool call types
interface ToolCallParams {
  name: string
  arguments: Record<string, unknown>
}

interface ToolCallResult {
  content: Array<{
    type: string
    text: string
  }>
  isError?: boolean
}

let requestId = 0

/**
 * Generate unique request ID.
 */
function nextId(): number {
  return ++requestId
}

/**
 * Call an MCP tool on the Vision Admin server.
 *
 * @param name - Tool name (e.g., "vision_list")
 * @param args - Tool arguments
 * @returns Tool result text
 * @throws Error if daemon is not running or tool call fails
 */
export async function callTool(name: string, args: Record<string, unknown> = {}): Promise<string> {
  const requestTimeoutMs = getVisionRequestTimeoutMs()
  const bodyTimeoutMs = getVisionBodyReadTimeoutMs()
  const controller = new AbortController()
  const timeoutId = setTimeout(() => controller.abort(), requestTimeoutMs)

  const request: JsonRpcRequest = {
    jsonrpc: "2.0",
    method: "tools/call",
    params: {
      name,
      arguments: args,
    } satisfies ToolCallParams,
    id: nextId(),
  }

  try {
    const response = await fetch(MCP_ENDPOINT, {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
      },
      body: JSON.stringify(request),
      signal: controller.signal,
    })

    clearTimeout(timeoutId)

    if (!response.ok) {
      throw new Error(`HTTP error: ${response.status}`)
    }

    const rpcResponse = await readJsonWithTimeout<JsonRpcResponse>(response, bodyTimeoutMs)

    // Return MCP errors as JSON error objects per spec
    if (rpcResponse.error) {
      return JSON.stringify({
        success: false,
        error: rpcResponse.error.message,
        code: rpcResponse.error.code,
      })
    }

    const result = rpcResponse.result as ToolCallResult

    // Extract text from content - this is the JSON response from the tool
    // Tool errors are now returned as part of the JSON (success: false)
    const text = result.content
      ?.filter((c) => c.type === "text")
      .map((c) => c.text)
      .join("\n")

    return text || "{}"
  } catch (err) {
    clearTimeout(timeoutId)

    // Return connection errors as JSON error objects per spec
    if (err instanceof Error) {
      if (err.name === "AbortError") {
        return JSON.stringify({
          success: false,
          error: `Vision daemon request timeout after ${requestTimeoutMs}ms`,
        })
      }
      if (err.message.includes("ECONNREFUSED") || err.message.includes("fetch failed")) {
        return JSON.stringify({
          success: false,
          error: "Vision daemon is not running",
          suggestion: "Start it with: vision daemon start",
        })
      }
      return JSON.stringify({
        success: false,
        error: err.message,
      })
    }

    return JSON.stringify({
      success: false,
      error: "Unknown error calling Vision daemon",
    })
  }
}

/**
 * Initialize the MCP connection (calls initialize method).
 * Optional - the server handles tools/call without explicit initialization.
 */
export async function initialize(): Promise<void> {
  const requestTimeoutMs = getVisionRequestTimeoutMs()
  const bodyTimeoutMs = getVisionBodyReadTimeoutMs()
  const controller = new AbortController()
  const timeoutId = setTimeout(() => controller.abort(), requestTimeoutMs)

  const request: JsonRpcRequest = {
    jsonrpc: "2.0",
    method: "initialize",
    params: {},
    id: nextId(),
  }

  try {
    const response = await fetch(MCP_ENDPOINT, {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
      },
      body: JSON.stringify(request),
      signal: controller.signal,
    })

    clearTimeout(timeoutId)

    if (!response.ok) {
      throw new Error(`HTTP error: ${response.status}`)
    }

    const rpcResponse = await readJsonWithTimeout<JsonRpcResponse>(response, bodyTimeoutMs)

    if (rpcResponse.error) {
      throw new Error(`MCP initialize error: ${rpcResponse.error.message}`)
    }
  } catch (err) {
    clearTimeout(timeoutId)

    if (err instanceof Error) {
      if (err.name === "AbortError") {
        throw new Error(`Vision daemon request timeout after ${requestTimeoutMs}ms`)
      }
      if (err.message.includes("ECONNREFUSED") || err.message.includes("fetch failed")) {
        throw new Error("Vision daemon is not running. Start it with: vision daemon start")
      }
      throw err
    }

    throw new Error("Unknown error initializing Vision daemon connection")
  }
}

/**
 * List available tools from the Admin MCP server.
 */
export async function listTools(): Promise<string[]> {
  const requestTimeoutMs = getVisionRequestTimeoutMs()
  const bodyTimeoutMs = getVisionBodyReadTimeoutMs()
  const controller = new AbortController()
  const timeoutId = setTimeout(() => controller.abort(), requestTimeoutMs)

  const request: JsonRpcRequest = {
    jsonrpc: "2.0",
    method: "tools/list",
    params: {},
    id: nextId(),
  }

  try {
    const response = await fetch(MCP_ENDPOINT, {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
      },
      body: JSON.stringify(request),
      signal: controller.signal,
    })

    clearTimeout(timeoutId)

    if (!response.ok) {
      throw new Error(`HTTP error: ${response.status}`)
    }

    const rpcResponse = await readJsonWithTimeout<JsonRpcResponse>(response, bodyTimeoutMs)

    if (rpcResponse.error) {
      throw new Error(`MCP error: ${rpcResponse.error.message}`)
    }

    const result = rpcResponse.result as { tools: Array<{ name: string }> }
    return result.tools?.map((t) => t.name) || []
  } catch (err) {
    clearTimeout(timeoutId)

    if (err instanceof Error) {
      if (err.name === "AbortError") {
        throw new Error(`Vision daemon request timeout after ${requestTimeoutMs}ms`)
      }
      if (err.message.includes("ECONNREFUSED") || err.message.includes("fetch failed")) {
        throw new Error("Vision daemon is not running. Start it with: vision daemon start")
      }
      throw err
    }

    throw new Error("Unknown error listing Vision tools")
  }
}
