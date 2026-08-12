// Vision plugin tool-name constants.
//
// Data-only sibling module. Kept out of the plugin ENTRY module (index.ts)
// because the OpenCode 1.18.4+ plugin loader iterates
// `Object.values(entryModule)` and throws "Plugin export is not a function"
// for any export that is not a function or `{ server }` object. A plain object
// export in index.ts trips that check. index.ts and tests import from here.

export const VISION_DAEMON_TOOL_NAMES = {
  list: "vision_list",
  add: "vision_add",
  remove: "vision_remove",
  restart: "vision_restart",
  search: "vision_search",
  init: "vision_init",
  status: "vision_status",
  guidance: "vision_guidance",
  slotStatus: "vision_slot_status",
  metrics: "vision_metrics",
} as const

export const OPENCODE_MCP_TOOL_NAMES = {
  mcpConnect: "opencode_mcp_connect",
  mcpDisconnect: "opencode_mcp_disconnect",
} as const

export const VISION_PLUGIN_TOOL_NAMES = {
  ...VISION_DAEMON_TOOL_NAMES,
  ...OPENCODE_MCP_TOOL_NAMES,
} as const
