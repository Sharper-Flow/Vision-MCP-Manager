export interface VisionContextOptions {
  healthy: boolean
  codeMode: boolean
}

import { OPENCODE_MCP_TOOL_NAMES, VISION_DAEMON_TOOL_NAMES } from "./tool-names"

const REGISTRY_CONTROL_TOOLS = [
  [OPENCODE_MCP_TOOL_NAMES.mcpConnect, "Enable a declared OpenCode MCP server"],
  [OPENCODE_MCP_TOOL_NAMES.mcpDisconnect, "Disable a declared OpenCode MCP server"],
] as const

const DAEMON_BACKED_TOOLS = [
  [VISION_DAEMON_TOOL_NAMES.list, "List all registered servers with status"],
  [VISION_DAEMON_TOOL_NAMES.add, "Add and start an MCP server"],
  [VISION_DAEMON_TOOL_NAMES.remove, "Stop and remove an MCP server"],
  [VISION_DAEMON_TOOL_NAMES.restart, "Restart a server in-place without port drift"],
  [VISION_DAEMON_TOOL_NAMES.search, "Search for servers by name or capability"],
  [VISION_DAEMON_TOOL_NAMES.init, "Generate opencode.jsonc config"],
  [VISION_DAEMON_TOOL_NAMES.status, "Check daemon health"],
  [VISION_DAEMON_TOOL_NAMES.guidance, "Get ranked tool-selection guidance"],
  [VISION_DAEMON_TOOL_NAMES.slotStatus, "Inspect slot-group routing health"],
  [VISION_DAEMON_TOOL_NAMES.metrics, "Inspect daemon and tool-call metrics"],
] as const

function toolName(name: string, codeMode: boolean): string {
  return codeMode ? `tools.vision.${name}()` : name
}

function renderTools(healthy: boolean, codeMode: boolean): string {
  const registryControlTools = REGISTRY_CONTROL_TOOLS.map(
    ([name, description]) => `- **${name}** - ${description}`
  ).join("\n")
  const daemonTools = DAEMON_BACKED_TOOLS.map(([name, description]) => {
    const availability = healthy
      ? ""
      : " (daemon-backed tools are unavailable: Vision daemon is NOT running)"
    return `- **${toolName(name, codeMode)}** - ${description}${availability}`
  }).join("\n")

  return `## Registry-Control Tools\n\n${registryControlTools}\n\n## Daemon-Backed Tools\n\n${daemonTools}`
}

export function renderVisionContext({ healthy, codeMode }: VisionContextOptions): string {
  const search = toolName(VISION_DAEMON_TOOL_NAMES.search, codeMode)
  const add = toolName(VISION_DAEMON_TOOL_NAMES.add, codeMode)
  const init = toolName(VISION_DAEMON_TOOL_NAMES.init, codeMode)
  const discovery = codeMode
    ? `\n## Code Mode Discovery\n\nUse \`tools.$codemode.search({ query: "MCP server management", namespace: "vision" })\` to discover the current callable signatures.\n`
    : ""
  const status = healthy
    ? "Vision is running and ready to help you manage MCP servers."
    : "Vision daemon is NOT running. Daemon-backed tools are unavailable until the daemon starts; registry-control tools remain usable now."
  const startup = healthy
    ? ""
    : `
## To Start Vision

Run in your terminal:
\`\`\`bash
vision daemon start
\`\`\`

After startup, daemon-backed tools will be available. Registry-control tools remain usable now.

## Check Status

\`\`\`bash
vision daemon status
\`\`\`
`
  const quickStart = healthy
    ? `
## Quick Start

1. List available servers: Use ${toolName(VISION_DAEMON_TOOL_NAMES.list, codeMode)}
2. Add a server: Use ${add} with a server name
3. Generate config: Use ${init}

## Example Workflow

\`\`\`
1. ${search} query="documentation"  # Find documentation servers
2. ${add} name="context7"           # Add context7 server
3. ${init}                           # Generate config file
\`\`\`
`
    : ""

  return `
# Vision MCP Server Manager

${status}

## Available Tools

${renderTools(healthy, codeMode)}
${discovery}
${quickStart}
${startup}`
}
