import { describe, expect, it } from "vitest"
import { renderVisionContext } from "./context"

describe("renderVisionContext", () => {
  it("renders namespaced tools and discovery in Code Mode", () => {
    const context = renderVisionContext({ healthy: true, codeMode: true })

    expect(context).toContain("tools.vision.vision_list()")
    expect(context).toContain("tools.$codemode.search")
    expect(context).toContain('namespace: "vision"')
  })

  it("preserves top-level tool guidance outside Code Mode", () => {
    const context = renderVisionContext({ healthy: true, codeMode: false })

    expect(context).toContain("vision_list")
    expect(context).not.toContain("tools.vision.")
    expect(context).not.toContain("tools.$codemode.search")
  })

  it("annotates daemon-down guidance for Code Mode", () => {
    const context = renderVisionContext({ healthy: false, codeMode: true })

    expect(context).toContain("Vision daemon is NOT running")
    expect(context).toContain("tools.vision.*")
  })

  it("keeps daemon-down guidance generic outside Code Mode", () => {
    const context = renderVisionContext({ healthy: false, codeMode: false })

    expect(context).toContain("Vision daemon is NOT running")
    expect(context).toContain("vision_* tools")
    expect(context).not.toContain("tools.vision.*")
  })
})
