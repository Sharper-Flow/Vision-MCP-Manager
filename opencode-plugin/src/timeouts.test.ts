import { afterEach, describe, expect, it } from "vitest"
import {
  getVisionBodyReadTimeoutMs,
  getVisionHealthTimeoutMs,
  getVisionRequestTimeoutMs,
} from "./timeouts"

const originalEnv = { ...process.env }

afterEach(() => {
  process.env = { ...originalEnv }
})

describe("Vision timeout config", () => {
  it("uses defaults when env vars are unset", () => {
    delete process.env.VISION_REQUEST_TIMEOUT_MS
    delete process.env.VISION_BODY_TIMEOUT_MS
    delete process.env.VISION_HEALTH_TIMEOUT_MS

    expect(getVisionRequestTimeoutMs()).toBe(30000)
    expect(getVisionBodyReadTimeoutMs()).toBe(10000)
    expect(getVisionHealthTimeoutMs()).toBe(5000)
  })

  it("uses valid env overrides", () => {
    process.env.VISION_REQUEST_TIMEOUT_MS = "45000"
    process.env.VISION_BODY_TIMEOUT_MS = "12000"
    process.env.VISION_HEALTH_TIMEOUT_MS = "3500"

    expect(getVisionRequestTimeoutMs()).toBe(45000)
    expect(getVisionBodyReadTimeoutMs()).toBe(12000)
    expect(getVisionHealthTimeoutMs()).toBe(3500)
  })

  it("ignores invalid env overrides", () => {
    process.env.VISION_REQUEST_TIMEOUT_MS = "abc"
    process.env.VISION_BODY_TIMEOUT_MS = "0"
    process.env.VISION_HEALTH_TIMEOUT_MS = "-5"

    expect(getVisionRequestTimeoutMs()).toBe(30000)
    expect(getVisionBodyReadTimeoutMs()).toBe(10000)
    expect(getVisionHealthTimeoutMs()).toBe(5000)
  })
})
