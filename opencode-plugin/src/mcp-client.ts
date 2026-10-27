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

import { z } from "zod"
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

const ToolCallResultSchema = z.looseObject({
  content: z
    .array(
      z
        .looseObject({ type: z.string(), text: z.string().optional() })
        .refine(
          (block) => block.type !== "text" || typeof block.text === "string",
          "Text content must have a string text field"
        )
    )
    .optional(),
  structuredContent: z.record(z.string(), z.unknown()).optional(),
  isError: z.boolean().optional(),
})

const RpcResponseSchema = z
  .looseObject({
    jsonrpc: z.literal("2.0"),
    id: z.union([z.number(), z.string(), z.null()]),
    result: z.unknown().optional(),
    error: z
      .object({ code: z.number(), message: z.string(), data: z.unknown().optional() })
      .optional(),
  })
  .refine((rpc) => "result" in rpc !== "error" in rpc, "Expected exactly one result or error")

const VisionFailureSchema = z.looseObject({
  success: z.literal(false),
  error: z.string().min(1),
  code: z.union([z.number(), z.string()]).optional(),
  data: z.unknown().optional(),
  suggestion: z.string().optional(),
  // Daemon-owned failure identity and extension fields pass through unchanged.
  kind: z.string().optional(),
  isError: z.literal(true).optional(),
})

/**
 * Plugin wire contract: JSON object with a success discriminator. Successful
 * daemon fields stay at the root; failures keep error/code/data/suggestion and
 * extension fields. A successful call can report healthy:false or a server error.
 * V1 serializes this union; V2 returns it as output alongside the same JSON text.
 * Tool-specific payload validation happens before this transport-level union.
 */
export const VisionOutcomeSchema = z.discriminatedUnion("success", [
  z.looseObject({ success: z.literal(true) }),
  VisionFailureSchema,
])
type VisionOutcome = z.infer<typeof VisionOutcomeSchema>

function serializeOutcome(outcome: VisionOutcome): string {
  return JSON.stringify(VisionOutcomeSchema.parse(outcome))
}

// These are the daemon's admin response shapes (internal/admin/tools.go), not
// downstream vendor schemas. Keep additive fields, including nested extensions.
const named = z.looseObject({ name: z.string() })
const mutation = z.looseObject({
  success: z.literal(true),
  name: z.string(),
  status: z.string().optional(),
  error: z.string().nullable().optional(),
  port: z.number().nullable().optional(),
})
const DaemonSuccessSchemas: Record<string, z.ZodObject> = {
  vision_list: z.looseObject({
    servers: z.array(named.extend({ status: z.string(), process_state: z.string() })),
    slot_groups: z.array(
      named.extend({ group_port: z.number(), slots: z.array(z.string()).nullable() })
    ),
  }),
  vision_add: mutation,
  vision_remove: mutation,
  vision_restart: mutation,
  vision_search: z.looseObject({
    success: z.literal(true),
    results: z.array(
      named.extend({
        description: z.string(),
        capabilities: z.array(z.string()).nullable(),
        installed: z.boolean(),
      })
    ),
  }),
  vision_init: z.looseObject({
    success: z.literal(true),
    path: z.string(),
    servers: z.array(z.string()),
  }),
  vision_status: z.looseObject({
    healthy: z.boolean(),
    uptime: z.string(),
    servers: z.looseObject({ running: z.number(), stopped: z.number(), error: z.number() }),
    memory_mb: z.number(),
    warnings: z.array(z.string()).optional(),
  }),
  vision_guidance: z.looseObject({
    success: z.literal(true),
    global_guidance: z.string().optional(),
    servers: z.array(named).optional(),
    tools: z.array(named).optional(),
    context: z.string().optional(),
    error: z.string().nullable().optional(),
  }),
  vision_slot_status: z.looseObject({
    groups: z.array(
      z.looseObject({
        group_name: z.string(),
        slot_count: z.number(),
        slots: z
          .array(
            named.extend({
              port: z.number(),
              active_sessions: z.number(),
              max_sessions: z.number(),
              effective_status: z.string(),
            })
          )
          .nullable(),
      })
    ),
  }),
  vision_metrics: z.looseObject({
    sessions_active: z.number(),
    tool_calls_total: z.number(),
    errors_total: z.number(),
    subprocesses_active: z.number(),
  }),
}

class InvalidResultError extends Error {}

class JsonRpcError extends Error {
  constructor(
    readonly detail: NonNullable<JsonRpcResponse["error"]>,
    prefix: string
  ) {
    super(`${prefix}: ${detail.message}`)
  }
}

function validate<T>(schema: z.ZodType<T>, value: unknown, context: string): T {
  const parsed = schema.safeParse(value)
  if (!parsed.success) {
    throw new InvalidResultError(`Invalid Vision ${context}: ${parsed.error.message}`)
  }
  return parsed.data
}

function decodeToolResult(name: string, value: unknown): VisionOutcome {
  const result = validate(ToolCallResultSchema, value, "MCP tool result")
  const text =
    result.content
      ?.filter((c) => c.type === "text")
      .map((c) => c.text)
      .join("\n") ?? ""
  let payload: unknown = result.structuredContent
  if (payload === undefined && text.trim()) {
    try {
      payload = JSON.parse(text)
    } catch {
      if (!result.isError) throw new InvalidResultError("Invalid Vision tool text: expected JSON")
      // The daemon's MCP handler also emits plain text on execution errors.
    }
  }
  if (result.isError) {
    const fields =
      payload && typeof payload === "object" && !Array.isArray(payload)
        ? (payload as Record<string, unknown>)
        : {}
    return validate(
      VisionFailureSchema,
      {
        ...fields,
        success: false,
        error: fields.error ?? (text.trim() || "Vision MCP tool reported an error"),
        kind: fields.kind ?? "mcp_tool",
        isError: true,
      },
      "MCP tool failure"
    )
  }
  const fields = validate(z.record(z.string(), z.unknown()), payload, "tool payload")
  if (fields.success === false) return validate(VisionFailureSchema, fields, "daemon failure")
  if (fields.success !== undefined && fields.success !== true) {
    throw new InvalidResultError("Invalid Vision success discriminator")
  }
  const schema = Object.hasOwn(DaemonSuccessSchemas, name) ? DaemonSuccessSchemas[name] : undefined
  if (!schema)
    throw new InvalidResultError(`Invalid Vision tool result: unsupported admin tool ${name}`)
  return validate(
    VisionOutcomeSchema,
    {
      ...validate(schema, fields, `${name} payload`),
      success: true,
    },
    "outcome"
  )
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
  throw new InvalidResultError("Invalid Vision SSE response: no non-empty data: frame")
}

/**
 * Read a JSON-RPC response from either an SSE or JSON body.
 */
async function readRpcResponse(response: Response, timeoutMs: number): Promise<JsonRpcResponse> {
  const contentType = response.headers.get("content-type") || ""
  try {
    const payload = contentType.includes("text/event-stream")
      ? await readSsePayload<unknown>(response, timeoutMs)
      : await readJsonWithTimeout<unknown>(response, timeoutMs)
    return validate(RpcResponseSchema, payload, "JSON-RPC response")
  } catch (err) {
    if (err instanceof SyntaxError)
      throw new InvalidResultError(`Invalid Vision response JSON: ${err.message}`)
    throw err
  }
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
    throw new JsonRpcError(rpcResponse.error, "initialize JSON-RPC error")
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
    if (rpcError.message === 'method "tools/call" is invalid during session initialization') {
      return true
    }
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
 * @returns JSON serialization of VisionOutcomeSchema. structuredContent wins
 *          over presentation text; without it, text blocks must form a valid
 *          daemon JSON payload. Missing/malformed results fail without replay.
 */
export async function callTool(name: string, args: Record<string, unknown> = {}): Promise<string> {
  let isToolError = false
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
      return serializeOutcome({
        success: false,
        error: rpcResponse.error.message,
        code: rpcResponse.error.code,
        data: rpcResponse.error.data,
        kind: "json_rpc",
      })
    }

    isToolError =
      typeof rpcResponse.result === "object" &&
      rpcResponse.result !== null &&
      "isError" in rpcResponse.result &&
      rpcResponse.result.isError === true
    return serializeOutcome(decodeToolResult(name, rpcResponse.result))
  } catch (err) {
    // Validation never retries: a mutation may already have completed.
    if (err instanceof InvalidResultError) {
      return serializeOutcome({
        success: false,
        error: err.message,
        kind: "invalid_result",
        ...(isToolError ? { isError: true as const } : {}),
        suggestion:
          "Do not automatically repeat this call; a mutation may have completed. Inspect daemon state before deciding whether to retry.",
      })
    }
    if (err instanceof JsonRpcError) {
      return serializeOutcome({
        success: false,
        error: err.message,
        code: err.detail.code,
        data: err.detail.data,
        kind: "json_rpc",
      })
    }
    if (err instanceof Error) {
      if (err.name === "AbortError" || err.message === "Response body read timeout") {
        return serializeOutcome({
          success: false,
          kind: "timeout",
          error:
            err.name === "AbortError"
              ? `Vision daemon request timeout after ${getVisionRequestTimeoutMs()}ms`
              : err.message,
        })
      }
      if (err.message.includes("ECONNREFUSED") || err.message.includes("fetch failed")) {
        const cause = err.cause
        return serializeOutcome({
          success: false,
          kind: "connection",
          error: "Vision daemon is not running",
          data: {
            message: err.message,
            ...(cause &&
            typeof cause === "object" &&
            "code" in cause &&
            (typeof cause.code === "string" || typeof cause.code === "number")
              ? { code: cause.code }
              : {}),
          },
          suggestion: "Restart it with: systemctl --user restart vision.service",
        })
      }
      return serializeOutcome({
        success: false,
        kind: "transport",
        error: err.message,
      })
    }

    return serializeOutcome({
      success: false,
      kind: "transport",
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
