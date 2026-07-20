export interface VisionContextOptions {
  healthy: boolean
  codeMode: boolean
}

const MANAGED_TOOLS = [
  ["vision_list", "List all registered servers with status"],
  ["vision_add", "Add and start an MCP server"],
  ["vision_remove", "Stop and remove an MCP server"],
  ["vision_restart", "Restart a server in-place without port drift"],
  ["vision_search", "Search for servers by name or capability"],
  ["vision_init", "Generate .opencode.json config"],
  ["vision_status", "Check daemon health"],
] as const

function toolName(name: string, codeMode: boolean): string {
  return codeMode ? `tools.vision.${name}()` : name
}

function renderHealthy(codeMode: boolean): string {
  const tools = MANAGED_TOOLS.map(
    ([name, description]) => `- **${toolName(name, codeMode)}** - ${description}`
  ).join("\n")

  const discovery = codeMode
    ? `\n## Code Mode Discovery\n\nUse \`tools.$codemode.search({ query: "MCP server management", namespace: "vision" })\` when you need the current callable signatures. Bare top-level \`vision_*\` calls are unavailable in Code Mode sessions.\n`
    : ""

  const search = toolName("vision_search", codeMode)
  const add = toolName("vision_add", codeMode)
  const init = toolName("vision_init", codeMode)

  return `
# Vision MCP Server Manager

Vision is running and ready to help you manage MCP servers.

## Available Tools

Use these tools to manage MCP servers:

${tools}
${discovery}
## Quick Start

1. List available servers: Use ${toolName("vision_list", codeMode)}
2. Add a server: Use ${add} with a server name
3. Generate config: Use ${init}

## Example Workflow

\`\`\`
1. ${search} query="documentation"  # Find documentation servers
2. ${add} name="context7"           # Add context7 server
3. ${init}                           # Generate config file
\`\`\`
`
}

function renderNotRunning(codeMode: boolean): string {
  const toolGuidance = codeMode
    ? "Once started, use `tools.vision.*` tools for MCP server management."
    : "Once started, you can use vision_* tools to manage MCP servers."

  return `
# Vision MCP Server Manager

Vision daemon is NOT running. MCP server management is unavailable.

## To Start Vision

Run in your terminal:
\`\`\`bash
vision daemon start
\`\`\`

${toolGuidance}

## Check Status

\`\`\`bash
vision daemon status
\`\`\`
`
}

export function renderVisionContext({ healthy, codeMode }: VisionContextOptions): string {
  return healthy ? renderHealthy(codeMode) : renderNotRunning(codeMode)
}
