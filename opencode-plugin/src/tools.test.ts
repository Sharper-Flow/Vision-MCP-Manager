/**
 * Guard for the daemon-down suggestion surfaced by the vision_* tool gate.
 *
 * The daemon is supervised by systemd (vision.service). Agent-facing text must
 * never tell an agent to start the daemon with a bare `vision daemon start`,
 * which bypasses the unit's EnvironmentFile.
 */

import { beforeEach, describe, expect, it, vi } from "vitest"

const { isDaemonRunningMock, callToolMock } = vi.hoisted(() => ({
  isDaemonRunningMock: vi.fn(async () => false),
  callToolMock: vi.fn(async () => "{}"),
}))

vi.mock("./health", () => ({
  isDaemonRunning: isDaemonRunningMock,
}))

vi.mock("./mcp-client", () => ({
  callTool: callToolMock,
}))

import { visionList } from "./tools"

describe("daemon-down suggestion", () => {
  beforeEach(() => {
    isDaemonRunningMock.mockClear()
    callToolMock.mockClear()
  })

  it("names systemctl --user restart vision.service when the daemon is down", async () => {
    isDaemonRunningMock.mockResolvedValue(false)

    const result = JSON.parse(await visionList()) as {
      success: boolean
      suggestion: string
    }

    expect(result.success).toBe(false)
    expect(result.suggestion).toContain("systemctl --user restart vision.service")
    expect(result.suggestion).not.toContain("vision daemon start")
    expect(callToolMock).not.toHaveBeenCalled()
  })
})
