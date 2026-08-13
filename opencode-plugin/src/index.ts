/**
 * Vision Plugin for OpenCode
 *
 * Provides AI agents with the ability to discover and configure MCP servers
 * through the Vision daemon. This plugin:
 *
 * 1. Injects context at session start explaining how to use Vision
 * 2. Exposes Vision MCP tools for server management
 * 3. Handles daemon-not-running errors gracefully
 *
 * Architecture:
 * - All server management goes through the Admin MCP on port 6275
 * - The plugin is a thin wrapper that calls the MCP tools
 * - Context injection solves the "bootstrap problem"
 */

import { tool, type Plugin } from "@opencode-ai/plugin"
import { access, stat } from "node:fs/promises"
import { isAbsolute, resolve } from "node:path"
import { z } from "zod"
import { renderVisionContext } from "./context"
import { checkHealth } from "./health"
import {
  visionList,
  visionAdd,
  visionRemove,
  visionRestart,
  visionSearch,
  visionInit,
  visionStatus,
  visionGuidance,
  visionSlotStatus,
  visionMetrics,
  VisionAddArgsSchema,
  VisionRemoveArgsSchema,
  VisionRestartArgsSchema,
  VisionSearchArgsSchema,
  VisionInitArgsSchema,
  VisionGuidanceArgsSchema,
} from "./tools"
import {
  connectMcpServer,
  disconnectMcpServer,
  McpConnectArgsSchema,
  McpDisconnectArgsSchema,
} from "./mcp-control"
// Tool-name constants live in a data-only sibling module so this ENTRY module
// exports functions only. The OpenCode 1.18.4+ loader iterates
// Object.values(entryModule) and throws "Plugin export is not a function" for
// any non-function export (a plain object map trips it). Imported WITHOUT
// re-export; consumers/tests import from "./tool-names" directly.
import { VISION_PLUGIN_TOOL_NAMES } from "./tool-names"

// =============================================================================
// Event Schemas
// =============================================================================

const SessionCreatedPropsSchema = z.object({
  session: z.object({
    id: z.string(),
  }),
})

// =============================================================================
// Plugin State
// =============================================================================

interface PluginState {
  daemonHealthy: boolean
  lastHealthCheck: number
  healthCheckInProgress: boolean
}

// =============================================================================
// Plugin Entry Point
// =============================================================================

const VisionPlugin: Plugin = async ({ client, directory }) => {
  // Initialize state
  const state: PluginState = {
    daemonHealthy: false,
    lastHealthCheck: 0,
    healthCheckInProgress: false,
  }

  // Check daemon health on startup
  const initialHealth = await checkHealth()
  state.daemonHealthy = initialHealth.healthy
  state.lastHealthCheck = Date.now()

  return {
    // ===========================================================================
    // Event Hooks
    // ===========================================================================

    event: async (input) => {
      const { event } = input
      // Handle session.created to check daemon health
      if (event.type === "session.created") {
        const parsed = SessionCreatedPropsSchema.safeParse(event.properties)
        if (parsed.success) {
          // Check daemon health when session starts
          const health = await checkHealth()
          state.daemonHealthy = health.healthy
          state.lastHealthCheck = Date.now()
        }
      }
    },

    // ===========================================================================
    // Context Injection (Compaction Hook)
    // ===========================================================================

    "experimental.session.compacting": async (_input, output) => {
      // Check daemon health if we haven't recently (with debounce to prevent concurrent checks)
      const now = Date.now()
      if (now - state.lastHealthCheck > 30000 && !state.healthCheckInProgress) {
        state.healthCheckInProgress = true
        try {
          const health = await checkHealth()
          state.daemonHealthy = health.healthy
          state.lastHealthCheck = Date.now()
        } finally {
          state.healthCheckInProgress = false
        }
      }

      // Read Code Mode at render time so long-lived plugin hosts can follow
      // environment changes without reloading the module.
      const context = renderVisionContext({
        healthy: state.daemonHealthy,
        codeMode: process.env.OPENCODE_EXPERIMENTAL_CODE_MODE === "true",
      })

      // Push context into compaction output
      output.context.push(context)
    },

    // ===========================================================================
    // Tool Definitions
    // ===========================================================================
    //
    // OpenCode reads `tool` — a record keyed by tool name — from the Hooks
    // object. A `tools` array is not part of the contract and is silently
    // ignored, which is why none of these were ever registered. Each entry uses
    // the `tool()` helper from @opencode-ai/plugin so `args` (a bare
    // ZodRawShape, not a wrapped ZodObject) and `execute` are checked by the
    // owning mechanism rather than by hand. src/registration.test.ts guards the
    // shape. Handlers return strings, which satisfy ToolResult directly.

    tool: {
      [VISION_PLUGIN_TOOL_NAMES.list]: tool({
        description:
          "List all registered MCP servers with their current status (running/stopped/failed)",
        args: {},
        execute: async () => await visionList(),
      }),
      [VISION_PLUGIN_TOOL_NAMES.add]: tool({
        description: "Add and optionally start an MCP server from the Vision registry",
        args: VisionAddArgsSchema.shape,
        execute: async (args) => await visionAdd(args),
      }),
      [VISION_PLUGIN_TOOL_NAMES.remove]: tool({
        description: "Stop and remove an MCP server from the active configuration",
        args: VisionRemoveArgsSchema.shape,
        execute: async (args) => await visionRemove(args),
      }),
      [VISION_PLUGIN_TOOL_NAMES.restart]: tool({
        description:
          "Restart a configured MCP server in-place, preserving its port assignment. Use this instead of vision_remove + vision_add to avoid port drift on servers defined in servers.yaml.",
        args: VisionRestartArgsSchema.shape,
        execute: async (args) => await visionRestart(args),
      }),
      [VISION_PLUGIN_TOOL_NAMES.search]: tool({
        description:
          "Search the Vision registry for MCP servers by name, capability tags, or description",
        args: VisionSearchArgsSchema.shape,
        execute: async (args) => await visionSearch(args),
      }),
      [VISION_PLUGIN_TOOL_NAMES.init]: tool({
        description:
          "Generate OpenCode MCP configuration (opencode.jsonc or another recognized config path) for currently running servers",
        args: VisionInitArgsSchema.shape,
        execute: async (args) => {
          const path = await resolveInitPath(directory, args.path)
          return await visionInit({ ...args, path })
        },
      }),
      [VISION_PLUGIN_TOOL_NAMES.status]: tool({
        description: "Get Vision daemon status including uptime, memory usage, and server counts",
        args: {},
        execute: async () => await visionStatus(),
      }),
      [VISION_PLUGIN_TOOL_NAMES.guidance]: tool({
        description: "Get ranked tool-selection guidance for a task or specific server",
        args: VisionGuidanceArgsSchema.shape,
        execute: async (args) => await visionGuidance(args),
      }),
      [VISION_PLUGIN_TOOL_NAMES.slotStatus]: tool({
        description: "Get slot-group routing status and per-slot session details",
        args: {},
        execute: async () => await visionSlotStatus(),
      }),
      [VISION_PLUGIN_TOOL_NAMES.metrics]: tool({
        description: "Get Vision daemon metrics for sessions, tool calls, errors, and subprocesses",
        args: {},
        execute: async () => await visionMetrics(),
      }),
      [VISION_PLUGIN_TOOL_NAMES.mcpConnect]: tool({
        description:
          "Use this when you have determined you need an MCP server already declared in the OpenCode config `mcp` block but currently disabled or failed. This is a session-lifetime runtime connection: it edits no config file, does not persist, and ends when the opencode process ends. The registry change takes effect immediately, so tools resolved at call time are usable right away; the list of tools advertised for the current turn was fixed when the turn began, so a newly connected server may not appear there until your next turn. It cannot add or connect a server that is not declared in the `mcp` block.",
        args: McpConnectArgsSchema.shape,
        execute: async (args) => await connectMcpServer(client, args),
      }),
      [VISION_PLUGIN_TOOL_NAMES.mcpDisconnect]: tool({
        description:
          "Use this when you have determined you no longer need an MCP server that is already declared in the OpenCode config `mcp` block and currently connected. This is a session-lifetime runtime disconnection: it edits no config file, does not persist, and ends when the opencode process ends. The registry change takes effect immediately, so the server's tools stop being callable right away; the list of tools advertised for the current turn was fixed when the turn began, so it may still appear there until your next turn. It cannot remove or change a server declaration, and it cannot manage a server that is not declared in the `mcp` block.",
        args: McpDisconnectArgsSchema.shape,
        execute: async (args) => await disconnectMcpServer(client, args),
      }),
    },
  }
}

const INIT_CONFIG_CANDIDATES = [
  "opencode.jsonc",
  "opencode.json",
  ".opencode/opencode.jsonc",
  ".opencode/opencode.json",
] as const

async function existingConfigPath(path: string): Promise<boolean> {
  try {
    await access(path)
    const details = await stat(path)
    return details.isFile()
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === "ENOENT") return false
    throw error
  }
}

async function resolveInitPath(directory: string, explicitPath?: string): Promise<string> {
  if (explicitPath !== undefined) {
    return isAbsolute(explicitPath) ? explicitPath : resolve(directory, explicitPath)
  }

  const matches: string[] = []
  for (const candidate of INIT_CONFIG_CANDIDATES) {
    if (await existingConfigPath(resolve(directory, candidate))) {
      matches.push(candidate)
    }
  }
  if (matches.length > 1) {
    throw new Error(`Multiple OpenCode config candidates found: ${matches.join(", ")}`)
  }
  return resolve(directory, matches[0] ?? INIT_CONFIG_CANDIDATES[0])
}

export default VisionPlugin
