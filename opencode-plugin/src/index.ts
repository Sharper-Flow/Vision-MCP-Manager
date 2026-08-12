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

import type { Plugin } from "@opencode-ai/plugin"
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

const VisionPlugin: Plugin = async ({ client }) => {
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

    tools: [
      {
        name: VISION_PLUGIN_TOOL_NAMES.list,
        description:
          "List all registered MCP servers with their current status (running/stopped/failed)",
        parameters: z.object({}),
        execute: async () => {
          const result = await visionList()
          return { content: result }
        },
      },
      {
        name: VISION_PLUGIN_TOOL_NAMES.add,
        description: "Add and optionally start an MCP server from the Vision registry",
        parameters: VisionAddArgsSchema,
        execute: async (args: z.infer<typeof VisionAddArgsSchema>) => {
          const result = await visionAdd(args)
          return { content: result }
        },
      },
      {
        name: VISION_PLUGIN_TOOL_NAMES.remove,
        description: "Stop and remove an MCP server from the active configuration",
        parameters: VisionRemoveArgsSchema,
        execute: async (args: z.infer<typeof VisionRemoveArgsSchema>) => {
          const result = await visionRemove(args)
          return { content: result }
        },
      },
      {
        name: VISION_PLUGIN_TOOL_NAMES.restart,
        description:
          "Restart a configured MCP server in-place, preserving its port assignment. Use this instead of vision_remove + vision_add to avoid port drift on servers defined in servers.yaml.",
        parameters: VisionRestartArgsSchema,
        execute: async (args: z.infer<typeof VisionRestartArgsSchema>) => {
          const result = await visionRestart(args)
          return { content: result }
        },
      },
      {
        name: VISION_PLUGIN_TOOL_NAMES.search,
        description:
          "Search the Vision registry for MCP servers by name, capability tags, or description",
        parameters: VisionSearchArgsSchema,
        execute: async (args: z.infer<typeof VisionSearchArgsSchema>) => {
          const result = await visionSearch(args)
          return { content: result }
        },
      },
      {
        name: VISION_PLUGIN_TOOL_NAMES.init,
        description:
          "Generate MCP client configuration (.opencode.json) for currently running servers",
        parameters: VisionInitArgsSchema,
        execute: async (args: z.infer<typeof VisionInitArgsSchema>) => {
          const result = await visionInit(args)
          return { content: result }
        },
      },
      {
        name: VISION_PLUGIN_TOOL_NAMES.status,
        description: "Get Vision daemon status including uptime, memory usage, and server counts",
        parameters: z.object({}),
        execute: async () => {
          const result = await visionStatus()
          return { content: result }
        },
      },
      {
        name: VISION_PLUGIN_TOOL_NAMES.guidance,
        description: "Get ranked tool-selection guidance for a task or specific server",
        parameters: VisionGuidanceArgsSchema,
        execute: async (args: z.infer<typeof VisionGuidanceArgsSchema>) => {
          const result = await visionGuidance(args)
          return { content: result }
        },
      },
      {
        name: VISION_PLUGIN_TOOL_NAMES.slotStatus,
        description: "Get slot-group routing status and per-slot session details",
        parameters: z.object({}),
        execute: async () => {
          const result = await visionSlotStatus()
          return { content: result }
        },
      },
      {
        name: VISION_PLUGIN_TOOL_NAMES.metrics,
        description: "Get Vision daemon metrics for sessions, tool calls, errors, and subprocesses",
        parameters: z.object({}),
        execute: async () => {
          const result = await visionMetrics()
          return { content: result }
        },
      },
      {
        name: VISION_PLUGIN_TOOL_NAMES.mcpConnect,
        description:
          "Use this when you have determined you need an MCP server already declared in the OpenCode config `mcp` block but currently disabled or failed. This is a session-lifetime runtime connection: it edits no config file, does not persist, and ends when the opencode process ends. The server's tools become available on the following turn, not this one; do not call them immediately after connecting. It cannot add or connect a server that is not declared in the `mcp` block.",
        parameters: McpConnectArgsSchema,
        execute: async (args: z.infer<typeof McpConnectArgsSchema>) => {
          const result = await connectMcpServer(client, args)
          return { content: result }
        },
      },
      {
        name: VISION_PLUGIN_TOOL_NAMES.mcpDisconnect,
        description:
          "Use this when you have determined you no longer need an MCP server that is already declared in the OpenCode config `mcp` block and currently connected. This is a session-lifetime runtime disconnection: it edits no config file, does not persist, and ends when the opencode process ends. The server's tools are gone from the following turn, not this one; do not call them after disconnecting. It cannot remove or change a server declaration, and it cannot manage a server that is not declared in the `mcp` block.",
        parameters: McpDisconnectArgsSchema,
        execute: async (args: z.infer<typeof McpDisconnectArgsSchema>) => {
          const result = await disconnectMcpServer(client, args)
          return { content: result }
        },
      },
    ],
  }
}

export default VisionPlugin
