import { afterEach, expect, it, vi } from "vitest"
import VisionPlugin from "./index"
import * as rootEntry from "../index"
import { VISION_PLUGIN_TOOL_NAMES } from "./tool-names"

afterEach(() => vi.unstubAllEnvs())

it("exports the same plugin object from the V2 directory entry", () => {
  expect(Object.keys(rootEntry)).toEqual(["default"])
  expect(rootEntry.default).toBe(VisionPlugin)
})

it.each([false, true])(
  "registers V2 plugin tools for direct calls (Code Mode %s)",
  async (mode) => {
    vi.stubEnv("OPENCODE_EXPERIMENTAL_CODE_MODE", String(mode))
    const tools: { name: string; options?: { codemode?: boolean } }[] = []
    const context = {
      tool: {
        transform: async (edit: (editor: unknown) => void) =>
          edit({ add: (tool: unknown) => tools.push(tool as (typeof tools)[number]) }),
      },
    }
    await VisionPlugin.setup(context as never)
    expect(tools.length).toBe(mode ? 2 : Object.keys(VISION_PLUGIN_TOOL_NAMES).length)
    for (const tool of tools) expect(tool.options?.codemode, tool.name).toBe(false)
  }
)
