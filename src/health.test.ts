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
