# AI Agents and Vision

Vision is designed to be the central hub for AI agents interacting with the Model Context Protocol (MCP). It provides a stable, supervised environment for MCP servers and exposes a management interface that agents can use directly.

## How Agents Interact with Vision

Agents typically interact with Vision in two ways:

1.  **Consuming Tools**: The agent connects to Vision-managed HTTP endpoints (ports 6276-6325) to use specific MCP tools (e.g., Google Search, Filesystem, Time).
2.  **Managing Infrastructure**: The agent connects to Vision's Admin MCP server (port 6275) to manage the toolset itself—adding new servers, restarting failed ones, or searching for new capabilities.

## Admin MCP Tools (Port 6275)

By giving an agent access to Vision's Admin port, you empower it to maintain its own tool environment.

| Tool | Purpose |
| :--- | :--- |
| `vision_list` | Shows all configured servers and their current health. |
| `vision_status` | Returns daemon uptime and memory usage. |
| `vision_search` | Finds new MCP servers in the local catalog. |
| `vision_add` | Provisions a new server from the catalog. |
| `vision_remove` | Decommissions an existing server. |
| `vision_guidance` | Provides hints on which tools to prefer for specific tasks. |
| `vision_init` | Generates configuration files for other agents. |

## Supported Agents

### OpenCode

OpenCode supports Vision natively via its `remote` transport.

**Global Config (`~/.opencode.json`):**
```json
{
  "mcp": {
    "vision": {
      "type": "remote",
      "url": "http://localhost:6275/mcp",
      "enabled": true
    }
  }
}
```

#### OpenCode Code Mode

When `OPENCODE_EXPERIMENTAL_CODE_MODE=true`, MCP tools are called through the
Code Mode runtime rather than as top-level functions. Vision's Admin tools use
the `vision` namespace:

```javascript
const servers = await tools.vision.vision_list({})
const matches = await tools.vision.vision_search({ query: "documentation" })
```

Discover current signatures before calling an unfamiliar tool:

```javascript
return await tools.$codemode.search({
  query: "MCP server management",
  namespace: "vision",
})
```

Names containing characters that are not valid JavaScript identifiers use
bracket notation—for example `tools.context7["resolve-library-id"](...)`.
Host/plugin tools such as `adv_change_validate` are outside the confined Code
Mode runtime and keep their normal host-call form.

## Tool Selection Guidance

Vision's `vision_guidance` tool is a critical feature for advanced agents. It helps solve the "too many tools" problem by providing metadata about which tools are most reliable or appropriate for a given task.

When an agent is unsure which tool to use (e.g., "Should I use Brave Search or Kagi?"), it can call `vision_guidance(context="web search")` to receive ranked recommendations based on your local configuration. Guidance responses may include both the canonical name and a `namespaced_name` that can be copied into Code Mode.

## Best Practices for Agent Integration

1.  **Always enable the Admin server**: It allows the agent to self-heal if a managed server crashes.
2.  **Use descriptive names**: When adding servers via `vision_add`, use names that clearly indicate the tool's purpose to help the LLM's reasoning.
3.  **Monitor via `vision_status`**: If an agent feels "sluggish," checking memory usage via the admin tool can help diagnose resource constraints in the underlying MCP processes.
