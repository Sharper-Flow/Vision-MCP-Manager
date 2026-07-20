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
  VisionAddArgsSchema,
  VisionRemoveArgsSchema,
  VisionRestartArgsSchema,
  VisionSearchArgsSchema,
  VisionInitArgsSchema,
} from "./tools"

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

const VisionPlugin: Plugin = async () => {
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
        name: "vision_list",
        description:
          "List all registered MCP servers with their current status (running/stopped/failed)",
        parameters: z.object({}),
        execute: async () => {
          const result = await visionList()
          return { content: result }
        },
      },
      {
        name: "vision_add",
        description: "Add and optionally start an MCP server from the Vision registry",
        parameters: VisionAddArgsSchema,
        execute: async (args: z.infer<typeof VisionAddArgsSchema>) => {
          const result = await visionAdd(args)
          return { content: result }
        },
      },
      {
        name: "vision_remove",
        description: "Stop and remove an MCP server from the active configuration",
        parameters: VisionRemoveArgsSchema,
        execute: async (args: z.infer<typeof VisionRemoveArgsSchema>) => {
          const result = await visionRemove(args)
          return { content: result }
        },
      },
      {
        name: "vision_restart",
        description:
          "Restart a configured MCP server in-place, preserving its port assignment. Use this instead of vision_remove + vision_add to avoid port drift on servers defined in servers.yaml.",
        parameters: VisionRestartArgsSchema,
        execute: async (args: z.infer<typeof VisionRestartArgsSchema>) => {
          const result = await visionRestart(args)
          return { content: result }
        },
      },
      {
        name: "vision_search",
        description:
          "Search the Vision registry for MCP servers by name, capability tags, or description",
        parameters: VisionSearchArgsSchema,
        execute: async (args: z.infer<typeof VisionSearchArgsSchema>) => {
          const result = await visionSearch(args)
          return { content: result }
        },
      },
      {
        name: "vision_init",
        description:
          "Generate MCP client configuration (.opencode.json) for currently running servers",
        parameters: VisionInitArgsSchema,
        execute: async (args: z.infer<typeof VisionInitArgsSchema>) => {
          const result = await visionInit(args)
          return { content: result }
        },
      },
      {
        name: "vision_status",
        description: "Get Vision daemon status including uptime, memory usage, and server counts",
        parameters: z.object({}),
        execute: async () => {
          const result = await visionStatus()
          return { content: result }
        },
      },
    ],
  }
}

export default VisionPlugin
