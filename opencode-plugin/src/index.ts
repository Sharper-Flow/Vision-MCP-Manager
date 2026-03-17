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
import { checkHealth } from "./health"
import {
  visionList,
  visionAdd,
  visionRemove,
  visionSearch,
  visionInit,
  visionStatus,
  VisionAddArgsSchema,
  VisionRemoveArgsSchema,
  VisionSearchArgsSchema,
  VisionInitArgsSchema,
} from "./tools"

// =============================================================================
// Context Injection Content
// =============================================================================

const VISION_CONTEXT_HEALTHY = `
# Vision MCP Server Manager

Vision is running and ready to help you manage MCP servers.

## Available Tools

Use these tools to manage MCP servers:

- **vision_list** - List all registered servers with status
- **vision_add** - Add and start an MCP server
- **vision_remove** - Stop and remove a server
- **vision_search** - Search for servers by name or capability
- **vision_init** - Generate .opencode.json config
- **vision_status** - Check daemon health

## Quick Start

1. List available servers: Use vision_list
2. Add a server: Use vision_add with server name
3. Generate config: Use vision_init to create .opencode.json

## Example Workflow

\`\`\`
1. vision_search query="documentation"  # Find documentation servers
2. vision_add name="context7"           # Add context7 server
3. vision_init                          # Generate config file
\`\`\`
`

const VISION_CONTEXT_NOT_RUNNING = `
# Vision MCP Server Manager

Vision daemon is NOT running. MCP server management is unavailable.

## To Start Vision

Run in your terminal:
\`\`\`bash
vision daemon start
\`\`\`

Once started, you can use vision_* tools to manage MCP servers.

## Check Status

\`\`\`bash
vision daemon status
\`\`\`
`

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

      // Return appropriate context based on daemon status
      const context = state.daemonHealthy ? VISION_CONTEXT_HEALTHY : VISION_CONTEXT_NOT_RUNNING

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
