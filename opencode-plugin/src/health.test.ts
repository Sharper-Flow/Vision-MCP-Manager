import { afterEach, describe, expect, it, vi } from "vitest"
import { checkHealth } from "./health"

const originalEnv = { ...process.env }

afterEach(() => {
  process.env = { ...originalEnv }
  vi.useRealTimers()
  vi.unstubAllGlobals()
  vi.restoreAllMocks()
})

describe("checkHealth", () => {
  // Contract source: internal/admin/server.go handleHealth (lines 195-237).
  // Port 6275 is the Admin MCP, which replaced a legacy REST API that returned
  // {"status":"healthy","uptime":...}. Its removal is recorded in
  // internal/daemon/daemon.go:194; the current shape below carries neither
  // uptime nor port.
  //
  // These cases exist because the previous suite tested only the body-timeout
  // path, so nothing ever exercised a successful response and the stale
  // "healthy" literal survived unnoticed while gating all ten vision_* tools.

  function stubHealthResponse(body: unknown, ok = true, status = 200) {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue({
        ok,
        status,
        json: async () => body,
      })
    )
  }

  it('treats {"status":"ok"} as healthy', async () => {
    stubHealthResponse({ status: "ok" })

    await expect(checkHealth()).resolves.toEqual({ healthy: true })
  })

  it('treats {"status":"degraded"} as healthy and surfaces the server errors', async () => {
    // The daemon is reachable and its management tools work; individual
    // managed servers have failed. Gating the management tools off here would
    // disable the exact capability needed to diagnose the failure.
    stubHealthResponse({
      status: "degraded",
      errors: ["episode: exited", "lgrep: crashed"],
    })

    await expect(checkHealth()).resolves.toEqual({
      healthy: true,
      degraded: true,
      errors: ["episode: exited", "lgrep: crashed"],
    })
  })

  it("treats a 503 unhealthy response as not healthy", async () => {
    stubHealthResponse({ status: "unhealthy" }, false, 503)

    await expect(checkHealth()).resolves.toEqual({
      healthy: false,
      error: "Unexpected status: 503",
    })
  })

  it("fails closed on an unrecognized status rather than assuming healthy", async () => {
    stubHealthResponse({ status: "something-new" })

    await expect(checkHealth()).resolves.toEqual({
      healthy: false,
      error: 'Unrecognized daemon health status: "something-new"',
    })
  })

  it("fails closed when the status field is missing", async () => {
    stubHealthResponse({})

    await expect(checkHealth()).resolves.toEqual({
      healthy: false,
      error: "Unrecognized daemon health status: undefined",
    })
  })

  it("times out stalled response bodies", async () => {
    process.env.VISION_HEALTH_TIMEOUT_MS = "25"
    vi.useFakeTimers()

    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue({
        ok: true,
        json: () => new Promise(() => {}),
      })
    )

    const pending = checkHealth()
    await vi.advanceTimersByTimeAsync(30)

    await expect(pending).resolves.toEqual({
      healthy: false,
      error: "Connection timeout - daemon may be slow or unresponsive",
    })
  })
})
