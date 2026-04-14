/**
 * Vision Tool Wrappers
 *
 * Provides typed wrappers for all Vision Admin MCP tools.
 * These are exported as OpenCode plugin tools.
 */

import { z } from "zod"
import { callTool } from "./mcp-client"
import { isDaemonRunning } from "./health"

// =============================================================================
// Zod Schemas for Tool Arguments
// =============================================================================

export const VisionListArgsSchema = z.object({}).describe("List all registered MCP servers")

export const VisionAddArgsSchema = z.object({
  name: z.string().describe("Name of the server to add (must exist in registry)"),
  start: z.boolean().optional().default(true).describe("Whether to start the server after adding"),
})

export const VisionRemoveArgsSchema = z.object({
  name: z.string().describe("Name of the server to remove"),
})

export const VisionRestartArgsSchema = z.object({
  name: z.string().describe("Name of the server to restart"),
})

export const VisionSearchArgsSchema = z.object({
  query: z
    .string()
    .optional()
    .describe("Search query (matches name, description, or capability tags)"),
  capability: z
    .string()
    .optional()
    .describe("Filter by specific capability tag (e.g., 'documentation', 'web-scraping')"),
})

export const VisionInitArgsSchema = z.object({
  path: z
    .string()
    .optional()
    .default(".opencode.json")
    .describe("Path to write the configuration file"),
  servers: z
    .string()
    .optional()
    .describe("Comma-separated list of server names to include (default: all running)"),
})

export const VisionStatusArgsSchema = z.object({}).describe("Get Vision daemon status")

// =============================================================================
// Tool Implementations
// =============================================================================

/**
 * Check if daemon is running and return error JSON if not.
 * Returns null if daemon is running, error JSON string if not.
 */
async function checkDaemonRunning(): Promise<string | null> {
  const running = await isDaemonRunning()
  if (!running) {
    return JSON.stringify({
      success: false,
      error: "Vision daemon is not running",
      suggestion: "Start it with: vision daemon start",
    })
  }
  return null
}

/**
 * List all registered MCP servers with their status.
 */
export async function visionList(): Promise<string> {
  const err = await checkDaemonRunning()
  if (err) return err
  return callTool("vision_list", {})
}

/**
 * Add and optionally start an MCP server.
 */
export async function visionAdd(args: z.infer<typeof VisionAddArgsSchema>): Promise<string> {
  const err = await checkDaemonRunning()
  if (err) return err
  return callTool("vision_add", {
    name: args.name,
    start: args.start,
  })
}

/**
 * Stop and remove an MCP server.
 */
export async function visionRemove(args: z.infer<typeof VisionRemoveArgsSchema>): Promise<string> {
  const err = await checkDaemonRunning()
  if (err) return err
  return callTool("vision_remove", {
    name: args.name,
  })
}

/**
 * Restart a configured MCP server in-place, preserving its port assignment.
 * Use this instead of vision_remove + vision_add to avoid port drift on
 * servers defined in servers.yaml.
 */
export async function visionRestart(
  args: z.infer<typeof VisionRestartArgsSchema>
): Promise<string> {
  const err = await checkDaemonRunning()
  if (err) return err
  return callTool("vision_restart", {
    name: args.name,
  })
}

/**
 * Search the registry for MCP servers.
 */
export async function visionSearch(args: z.infer<typeof VisionSearchArgsSchema>): Promise<string> {
  const err = await checkDaemonRunning()
  if (err) return err
  return callTool("vision_search", {
    query: args.query,
    capability: args.capability,
  })
}

/**
 * Generate MCP client configuration for running servers.
 */
export async function visionInit(args: z.infer<typeof VisionInitArgsSchema>): Promise<string> {
  const err = await checkDaemonRunning()
  if (err) return err
  // Convert comma-separated servers string to array for the API
  const servers = args.servers
    ? args.servers
        .split(",")
        .map((s) => s.trim())
        .filter((s) => s)
    : undefined
  return callTool("vision_init", {
    path: args.path,
    servers: servers,
  })
}

/**
 * Get Vision daemon status.
 */
export async function visionStatus(): Promise<string> {
  const err = await checkDaemonRunning()
  if (err) return err
  return callTool("vision_status", {})
}
