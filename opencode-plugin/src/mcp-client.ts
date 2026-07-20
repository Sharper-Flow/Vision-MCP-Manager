/**
 * MCP Client for Vision Admin Server
 *
 * Implements the MCP Streamable HTTP transport (2025-03-26 revision):
 *   - initialize handshake with protocolVersion negotiation
 *   - capture Mcp-Session-Id from initialize response
 *   - POST notifications/initialized with that session id
 *   - reuse Mcp-Session-Id on every subsequent tools/call
 *   - accept either application/json or text/event-stream response bodies
 *   - single-flight session init (concurrent callers share one initialize)
 *   - retry once on session-expiry signals (HTTP 404, or the daemon's
 *     pre-dispatch rejection `method "..." is invalid during session
 *     initialization`)
 *
 * All JSON-RPC 2.0 over HTTP POST to /mcp endpoint.
 */

import { getVisionBodyReadTimeoutMs, getVisionRequestTimeoutMs } from "./timeouts"

const ADMIN_PORT = 6275
const MCP_ENDPOINT = `http://localhost:${ADMIN_PORT}/mcp`

// MCP Streamable HTTP revision this client targets. The daemon must negotiate
// this exact version (or a later revision this client accepts) at initialize.
// Per /docs/decisions/0001-mcp-2026-07-28-alignment.md, when the daemon upgrades
// to MCP 2026-07-28 the initialize/initialized handshake is removed entirely.
const PROTOCOL_VERSION = "2025-03-26"

const CLIENT_INFO = { name: "vision-plugin", version: "1.0.2" }

// JSON-RPC 2.0 types
interface JsonRpcRequest {
  jsonrpc: "2.0"
  method: string
  params?: unknown
  id: number | string
}

interface JsonRpcNotification {
  jsonrpc: "2.0"
  method: string
  params?: unknown
  // Notifications have NO `id` field — that's what makes them notifications.
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

interface InitializeResult {
  protocolVersion: string
  capabilities?: unknown
  serverInfo?: unknown
}

// =============================================================================
// Request ID counter (shared across all JSON-RPC requests)
// =============================================================================

let requestId = 0

function nextId(): number {
  return ++requestId
}

// =============================================================================
// Session state
// =============================================================================

let cachedSessionId: string | null = null
let inFlightInit: Promise<string> | null = null

/**
 * Reset cached session state. Called when a session is known to be invalid
 * (HTTP 404, daemon-side session-initialization rejection). The next
 * ensureSession() will perform a fresh initialize.
 */
function resetSession(): void {
  cachedSessionId = null
}

async function readWithTimeout<T>(operation: Promise<T>, timeoutMs: number): Promise<T> {
  let timeoutId: ReturnType<typeof setTimeout> | undefined
  try {
    return await Promise.race([
      operation,
      new Promise<T>((_, reject) => {
        timeoutId = setTimeout(() => reject(new Error("Response body read timeout")), timeoutMs)
      }),
    ])
  } finally {
    if (timeoutId) clearTimeout(timeoutId)
  }
}

/**
 * Read response JSON with timeout protection.
 */
async function readJsonWithTimeout<T>(response: Response, timeoutMs: number): Promise<T> {
  return readWithTimeout(response.json() as Promise<T>, timeoutMs)
}

/**
 * Parse an SSE response body and return the JSON payload of the first
 * `data:` frame. Throws if no `data:` frame is present.
 *
 * MCP Streamable HTTP servers may respond with `text/event-stream` even for
 * single-response POSTs (initialize, tools/call). We only need the first
 * frame because each POST we make expects a single response.
 */
async function readSsePayload<T>(response: Response, timeoutMs: number): Promise<T> {
  const text = await readWithTimeout(response.text(), timeoutMs)
  for (const rawLine of text.split("\n")) {
    const line = rawLine.replace(/\r$/, "")
    if (line.startsWith("data:")) {
      const payload = line.slice("data:".length).trim()
      if (!payload) continue
      return JSON.parse(payload) as T
    }
  }
  throw new Error("SSE response had no non-empty data: frame")
}

/**
 * Read a JSON-RPC response from either an SSE or JSON body.
 */
async function readRpcResponse(response: Response, timeoutMs: number): Promise<JsonRpcResponse> {
  const contentType = response.headers.get("content-type") || ""
  if (contentType.includes("text/event-stream")) {
    return readSsePayload<JsonRpcResponse>(response, timeoutMs)
  }
  return readJsonWithTimeout<JsonRpcResponse>(response, timeoutMs)
}

/**
 * Perform the MCP initialize handshake.
 *
 * POST initialize → capture Mcp-Session-Id → POST notifications/initialized.
 * Returns the session ID. Does NOT cache (the caller is responsible for
 * caching via the inFlightInit promise below).
 *
 * Throws on:
 *   - HTTP error (non-2xx that isn't recoverable)
 *   - missing Mcp-Session-Id header
 *   - protocolVersion mismatch
 */
async function performInitialize(): Promise<string> {
  const requestTimeoutMs = getVisionRequestTimeoutMs()
  const bodyTimeoutMs = getVisionBodyReadTimeoutMs()
  const controller = new AbortController()
  const timeoutId = setTimeout(() => controller.abort(), requestTimeoutMs)

  const initReq: JsonRpcRequest = {
    jsonrpc: "2.0",
    method: "initialize",
    params: {
      protocolVersion: PROTOCOL_VERSION,
      capabilities: {},
      clientInfo: CLIENT_INFO,
    },
    id: nextId(),
  }

  let response: Response
  try {
    response = await fetch(MCP_ENDPOINT, {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        Accept: "application/json, text/event-stream",
      },
      body: JSON.stringify(initReq),
      signal: controller.signal,
    })
  } finally {
    clearTimeout(timeoutId)
  }

  if (!response.ok) {
    throw new Error(`initialize HTTP error: ${response.status}`)
  }

  const sessionId = response.headers.get("mcp-session-id")
  if (!sessionId) {
    throw new Error("daemon did not return Mcp-Session-Id on initialize")
  }

  const rpcResponse = await readRpcResponse(response, bodyTimeoutMs)

  if (rpcResponse.error) {
    throw new Error(`initialize JSON-RPC error: ${rpcResponse.error.message}`)
  }

  const initResult = rpcResponse.result as InitializeResult | undefined
  const negotiated = initResult?.protocolVersion
  if (negotiated !== PROTOCOL_VERSION) {
    throw new Error(
      `protocolVersion mismatch: requested ${PROTOCOL_VERSION}, daemon negotiated ${negotiated ?? "<missing>"}`
    )
  }

  // POST notifications/initialized (no `id` — it's a notification, no response expected).
  // Daemon responds with HTTP 202 and no body.
  const notifController = new AbortController()
  const notifTimeoutId = setTimeout(() => notifController.abort(), requestTimeoutMs)
  const notif: JsonRpcNotification = {
    jsonrpc: "2.0",
    method: "notifications/initialized",
    params: {},
  }

  try {
    const notificationResponse = await fetch(MCP_ENDPOINT, {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        Accept: "application/json, text/event-stream",
        "Mcp-Session-Id": sessionId,
      },
      body: JSON.stringify(notif),
      signal: notifController.signal,
    })
    if (!notificationResponse.ok) {
      throw new Error(`notifications/initialized HTTP error: ${notificationResponse.status}`)
    }
  } finally {
    clearTimeout(notifTimeoutId)
  }

  return sessionId
}

/**
 * Ensure a session is established. Single-flight: concurrent callers share
 * the same in-flight initialize promise.
 *
 * Returns the cached session ID if one is already established.
 */
export async function ensureSession(): Promise<string> {
  if (cachedSessionId) return cachedSessionId
  if (inFlightInit) return inFlightInit

  const init = (async () => {
    try {
      const sid = await performInitialize()
      cachedSessionId = sid
      return sid
    } finally {
      inFlightInit = null
    }
  })()

  inFlightInit = init
  return init
}

/**
 * For tests / external callers that need to inspect current session state.
 */
export function getSessionId(): string | null {
  return cachedSessionId
}

/**
 * Classify a JSON-RPC error response or HTTP status to decide whether a
 * single re-initialize+retry is appropriate.
 *
 * Spec-portable signal: HTTP 404 (MCP-defined session-expiry).
 * Daemon-specific signal: JSON-RPC error code 0 with message containing
 *   "is invalid during session initialization" — verified to be emitted by
 *   the Vision daemon's streamable handler strictly BEFORE tool dispatch
 *   (no side effects). See live evidence in change docs.
 *
 * Because postToolCall converts HTTP 404 into a synthetic JSON-RPC error
 * response (so the rest of the callTool pipeline is uniform), the classifier
 * must recognize BOTH the original HTTP status AND the synthetic message.
 */
function isSessionExpiredSignal(
  httpStatus: number,
  rpcError?: { code: number; message: string }
): boolean {
  if (httpStatus === 404) return true
  if (rpcError && rpcError.code === 0) {
    if (/is invalid during session initialization/i.test(rpcError.message)) return true
    if (/^HTTP 404/.test(rpcError.message)) return true
  }
  return false
}

/**
 * Perform a single tools/call attempt against the daemon. Throws on transport
 * errors; returns the raw JSON-RPC response on success or session-expiry signal.
 */
async function postToolCall(
  name: string,
  args: Record<string, unknown>,
  sessionId: string
): Promise<JsonRpcResponse> {
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

  let response: Response
  try {
    response = await fetch(MCP_ENDPOINT, {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        Accept: "application/json, text/event-stream",
        "Mcp-Session-Id": sessionId,
      },
      body: JSON.stringify(request),
      signal: controller.signal,
    })
  } finally {
    clearTimeout(timeoutId)
  }

  // HTTP 404 is the spec-defined session-expiry signal — surface it so the
  // caller can re-init and retry.
  if (response.status === 404) {
    return {
      jsonrpc: "2.0",
      id: request.id,
      error: { code: 0, message: "HTTP 404: session expired" },
    }
  }

  if (!response.ok) {
    throw new Error(`HTTP error: ${response.status}`)
  }

  return readRpcResponse(response, bodyTimeoutMs)
}

/**
 * Call an MCP tool on the Vision Admin server.
 *
 * Implements the full session lifecycle:
 *   1. ensureSession() — initialize + capture Mcp-Session-Id
 *   2. POST tools/call carrying Mcp-Session-Id
 *   3. On session-expiry signal (HTTP 404 or pre-dispatch rejection),
 *      reset session, re-initialize, retry once. Max 1 retry; no loops.
 *
 * @param name - Tool name (e.g., "vision_list")
 * @param args - Tool arguments
 * @returns Tool result text (JSON string). Errors are returned as JSON
 *          objects with `{ success: false, error: ... }` — same shape as
 *          pre-refactor behavior so callers (tools.ts) don't change.
 */
export async function callTool(name: string, args: Record<string, unknown> = {}): Promise<string> {
  try {
    let sessionId = await ensureSession()

    let rpcResponse = await postToolCall(name, args, sessionId)

    // Retry once on session-expiry signal.
    if (rpcResponse.error && isSessionExpiredSignal(200, rpcResponse.error)) {
      resetSession()
      sessionId = await ensureSession()
      rpcResponse = await postToolCall(name, args, sessionId)
    }

    // MCP-level errors are returned as JSON error objects per spec.
    if (rpcResponse.error) {
      return JSON.stringify({
        success: false,
        error: rpcResponse.error.message,
        code: rpcResponse.error.code,
      })
    }

    const result = rpcResponse.result as ToolCallResult

    // Extract text from content — this is the JSON response from the tool.
    // Tool errors are returned as part of the JSON (success: false).
    const text = result.content
      ?.filter((c) => c.type === "text")
      .map((c) => c.text)
      .join("\n")

    return text || "{}"
  } catch (err) {
    // Connection / transport errors as JSON error objects per spec.
    if (err instanceof Error) {
      if (err.name === "AbortError") {
        return JSON.stringify({
          success: false,
          error: `Vision daemon request timeout after ${getVisionRequestTimeoutMs()}ms`,
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

// =============================================================================
// Backward-compat: initialize() and listTools() exports
// =============================================================================

/**
 * Initialize the MCP connection.
 *
 * Historical note: prior to the session-lifecycle refactor, this performed a
 * bare initialize without capturing Mcp-Session-Id. The new behavior is a
 * thin wrapper around ensureSession() so external callers (if any) get the
 * correct session management. tools.ts does NOT call this — callTool handles
 * it internally.
 */
export async function initialize(): Promise<void> {
  await ensureSession()
}

/**
 * List available tools from the Admin MCP server.
 *
 * Uses the session-managed path so the call succeeds against the current
 * daemon. Returned as a JSON string for backward-compat with old callers.
 */
export async function listTools(): Promise<string[]> {
  const sessionId = await ensureSession()

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
        Accept: "application/json, text/event-stream",
        "Mcp-Session-Id": sessionId,
      },
      body: JSON.stringify(request),
      signal: controller.signal,
    })

    clearTimeout(timeoutId)

    if (!response.ok) {
      throw new Error(`HTTP error: ${response.status}`)
    }

    const rpcResponse = await readRpcResponse(response, bodyTimeoutMs)

    if (rpcResponse.error) {
      throw new Error(`MCP error: ${rpcResponse.error.message}`)
    }

    const result = rpcResponse.result as { tools: Array<{ name: string }> }
    return result.tools?.map((t) => t.name) || []
  } catch (err) {
    clearTimeout(timeoutId)
    throw err
  }
}
