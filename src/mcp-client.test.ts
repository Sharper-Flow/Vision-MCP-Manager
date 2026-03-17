import { afterEach, describe, expect, it, vi } from "vitest"
import { callTool } from "./mcp-client"

const originalEnv = { ...process.env }

afterEach(() => {
  process.env = { ...originalEnv }
  vi.unstubAllGlobals()
  vi.restoreAllMocks()
})

describe("callTool", () => {
  it("surfaces configured request timeout in abort errors", async () => {
    process.env.VISION_REQUEST_TIMEOUT_MS = "45000"

    const abortError = new Error("aborted")
    abortError.name = "AbortError"

    vi.stubGlobal("fetch", vi.fn().mockRejectedValue(abortError))

    const result = await callTool("vision_status")
    expect(JSON.parse(result)).toEqual({
      success: false,
      error: "Vision daemon request timeout after 45000ms",
    })
  })
})
