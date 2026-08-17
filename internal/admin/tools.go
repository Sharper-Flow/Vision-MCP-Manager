package admin

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/catalog"
	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/config"
	visionmcp "github.com/Sharper-Flow/Vision-MCP-Manager/internal/mcp"
	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/metrics"
	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/server"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tailscale/hujson"
)

// --- MCP Tool Definitions ---

// Tool represents an MCP tool definition.
type Tool struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	InputSchema InputSchema `json:"inputSchema"`
}

// InputSchema defines the JSON schema for tool inputs.
type InputSchema struct {
	Type       string              `json:"type"`
	Properties map[string]Property `json:"properties,omitempty"`
	Required   []string            `json:"required,omitempty"`
}

// Property defines a single property in the input schema.
type Property struct {
	Type        string `json:"type"`
	Description string `json:"description"`
	Default     any    `json:"default,omitempty"`
}

// getTools returns all available tools.
func (s *Server) getTools() []Tool {
	return []Tool{
		{
			Name:        "vision_list",
			Description: "List all registered MCP servers with their current effective status (running/starting/stopped/error)",
			InputSchema: InputSchema{
				Type:       "object",
				Properties: map[string]Property{},
			},
		},
		{
			Name:        "vision_add",
			Description: "Add and optionally start an MCP server from the registry",
			InputSchema: InputSchema{
				Type: "object",
				Properties: map[string]Property{
					"name": {
						Type:        "string",
						Description: "Name of the server to add (must exist in registry)",
					},
					"start": {
						Type:        "boolean",
						Description: "Whether to start the server after adding (default: true)",
						Default:     true,
					},
				},
				Required: []string{"name"},
			},
		},
		{
			Name:        "vision_remove",
			Description: "Stop and remove an MCP server from the active configuration",
			InputSchema: InputSchema{
				Type: "object",
				Properties: map[string]Property{
					"name": {
						Type:        "string",
						Description: "Name of the server to remove",
					},
				},
				Required: []string{"name"},
			},
		},
		{
			Name:        "vision_restart",
			Description: "Restart a configured MCP server in-place, preserving its port assignment. Use this instead of vision_remove + vision_add to avoid port drift on servers defined in servers.yaml.",
			InputSchema: InputSchema{
				Type: "object",
				Properties: map[string]Property{
					"name": {
						Type:        "string",
						Description: "Name of the server to restart",
					},
				},
				Required: []string{"name"},
			},
		},
		{
			Name:        "vision_search",
			Description: "Search the registry for MCP servers by name, capability tags, or description",
			InputSchema: InputSchema{
				Type: "object",
				Properties: map[string]Property{
					"query": {
						Type:        "string",
						Description: "Search query (matches name, description, or capability tags)",
					},
					"capability": {
						Type:        "string",
						Description: "Filter by specific capability tag (e.g., 'documentation', 'web-scraping')",
					},
				},
			},
		},
		{
			Name:        "vision_init",
			Description: "Generate an OpenCode MCP client configuration for running servers",
			InputSchema: InputSchema{
				Type: "object",
				Properties: map[string]Property{
					"path": {
						Type:        "string",
						Description: "Path to an OpenCode configuration file to write",
					},
					"servers": {
						Type:        "string",
						Description: "Comma-separated list of server names to include (default: all running)",
					},
				},
				Required: []string{"path"},
			},
		},
		{
			Name:        "vision_status",
			Description: "Get Vision daemon status including uptime, memory usage, and server counts",
			InputSchema: InputSchema{
				Type:       "object",
				Properties: map[string]Property{},
			},
		},
		{
			Name:        "vision_guidance",
			Description: "Get tool selection guidance and priorities for MCP servers. Use this to understand which tools to prefer for different tasks (e.g., use Kagi for web search, Context7 for docs, avoid Playwright for general browsing).",
			InputSchema: InputSchema{
				Type: "object",
				Properties: map[string]Property{
					"context": {
						Type:        "string",
						Description: "Optional task context to get relevant guidance (e.g., 'web search', 'documentation lookup', 'browser automation')",
					},
					"server": {
						Type:        "string",
						Description: "Optional server name to get specific guidance for",
					},
				},
			},
		},
		{
			Name:        "vision_slot_status",
			Description: "List all slot groups with per-slot session details: name, port, active_sessions, max_sessions. Read-only operator tool for monitoring slot-group routing health.",
			InputSchema: InputSchema{
				Type:       "object",
				Properties: map[string]Property{},
			},
		},
		{
			Name:        "vision_metrics",
			Description: "Get Vision daemon metrics including active sessions, tool calls, errors, and subprocess counts",
			InputSchema: InputSchema{
				Type:       "object",
				Properties: map[string]Property{},
			},
		},
	}
}

// ToolCallResult is the response to tools/call.
type ToolCallResult struct {
	Content []ToolContent `json:"content"`
	IsError bool          `json:"isError,omitempty"`
}

// ToolContent is a single piece of content in a tool result.
type ToolContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func (s *Server) registerTools(server *mcp.Server) {
	for _, tool := range s.getTools() {
		tool := tool
		schema, err := tool.InputSchema.marshal()
		if err != nil {
			schema = json.RawMessage(`{"type":"object"}`)
			s.logger.Warn("failed to marshal tool input schema",
				slog.String("tool", tool.Name),
				slog.String("error", err.Error()),
			)
		}

		server.AddTool(&mcp.Tool{
			Name:        tool.Name,
			Description: tool.Description,
			InputSchema: schema,
		}, s.toolHandler(tool.Name))
	}
}

func (s InputSchema) marshal() (json.RawMessage, error) {
	b, err := json.Marshal(s)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(b), nil
}

func (s *Server) toolHandler(name string) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.Params.Arguments
		if len(args) == 0 {
			args = json.RawMessage(`{}`)
		}

		result, err := s.callTool(ctx, name, args)
		if err != nil {
			if isValidationError(err) {
				return nil, fmt.Errorf("invalid params: %w", err)
			}
			return &mcp.CallToolResult{
				IsError: true,
				Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}},
			}, nil
		}

		return toMCPToolResult(result), nil
	}
}

func toMCPToolResult(result *ToolCallResult) *mcp.CallToolResult {
	if result == nil {
		return &mcp.CallToolResult{}
	}

	content := make([]mcp.Content, 0, len(result.Content))
	for _, c := range result.Content {
		content = append(content, &mcp.TextContent{Text: c.Text})
	}

	return &mcp.CallToolResult{
		Content: content,
		IsError: result.IsError,
	}
}

// ValidationError is returned for invalid tool arguments.
type ValidationError struct {
	msg string
}

func (e *ValidationError) Error() string {
	return e.msg
}

// NewValidationError creates a new validation error.
func NewValidationError(msg string) error {
	return &ValidationError{msg: msg}
}

// isValidationError checks if an error is a validation error.
func isValidationError(err error) bool {
	_, ok := err.(*ValidationError)
	return ok
}

// callTool dispatches to the appropriate tool handler.
func (s *Server) callTool(ctx context.Context, name string, args json.RawMessage) (*ToolCallResult, error) {
	switch name {
	case "vision_list":
		return s.toolList(ctx, args)
	case "vision_add":
		return s.toolAdd(ctx, args)
	case "vision_remove":
		return s.toolRemove(ctx, args)
	case "vision_restart":
		return s.toolRestart(ctx, args)
	case "vision_search":
		return s.toolSearch(ctx, args)
	case "vision_init":
		return s.toolInit(ctx, args)
	case "vision_status":
		return s.toolStatus(ctx, args)
	case "vision_guidance":
		return s.toolGuidance(ctx, args)
	case "vision_slot_status":
		return s.toolSlotStatus(ctx, args)
	case "vision_metrics":
		return s.toolMetrics(ctx, args)
	default:
		return nil, fmt.Errorf("unknown tool: %s", name)
	}
}

// --- Tool Implementations ---

// ListServerEntry represents a server in the vision_list response.
type ListServerEntry struct {
	Name              string  `json:"name"`
	CodemodeNamespace string  `json:"codemode_namespace"`
	Status            string  `json:"status"`
	EffectiveReason   *string `json:"effective_reason,omitempty"`
	ProcessState      string  `json:"process_state"`
	ReachabilityDetails
	Port             *int                           `json:"port"`
	PID              *int                           `json:"pid"`
	Uptime           *string                        `json:"uptime"`
	Error            *string                        `json:"error"`
	SessionMetrics   *metrics.ServerMetricsSnapshot `json:"session_metrics,omitempty"`
	SessionLifecycle *SessionLifecycleSnapshot      `json:"session_lifecycle,omitempty"`
}

// SlotGroupEntry describes one slot group in the vision_list response.
type SlotGroupEntry struct {
	Name      string   `json:"name"`
	GroupPort int      `json:"group_port"`
	Slots     []string `json:"slots"`
}

// ListResponse is the response for vision_list.
type ListResponse struct {
	Servers    []ListServerEntry `json:"servers"`
	SlotGroups []SlotGroupEntry  `json:"slot_groups"`
}

// toolList implements vision_list.
func (s *Server) toolList(ctx context.Context, args json.RawMessage) (*ToolCallResult, error) {
	if s.registry == nil {
		return nil, fmt.Errorf("registry not initialized")
	}

	servers := s.registry.List()
	response := ListResponse{
		Servers: make([]ListServerEntry, 0, len(servers)),
	}

	for _, srv := range servers {
		status := srv.Status()
		lifecycle := s.lifecycleSnapshot(status.Name)
		effective := deriveEffectiveStatus(status.State, lifecycleBackendState(lifecycle), s.reachabilityFor(status.Name), status.Transport.IsReachabilityProbeable(), status.Uptime, s.reachabilityGrace, status.LastError)
		info := ListServerEntry{
			Name:                status.Name,
			CodemodeNamespace:   s.codemodeNamespace(status.Name),
			Status:              effective.Status,
			ProcessState:        string(status.State),
			ReachabilityDetails: effective.Reachability,
		}

		// Set port if available
		if status.Port > 0 {
			port := status.Port
			info.Port = &port
		}

		// Set PID if running
		if status.PID > 0 {
			pid := status.PID
			info.PID = &pid
		}

		// Set uptime if running
		if status.Uptime > 0 {
			uptime := status.Uptime.Round(time.Second).String()
			info.Uptime = &uptime
		}

		// Set error if present
		if status.LastError != "" {
			errStr := scrubSecrets(status.LastError)
			info.Error = &errStr
		}

		// Set session metrics if accessor available
		if s.serverMetricsAccessor != nil {
			if snap := s.serverMetricsAccessor.ServerMetricsSnapshot(status.Name); snap != nil {
				info.SessionMetrics = snap
			}
		}
		if lifecycle != nil {
			info.SessionLifecycle = lifecycle
		}
		if effective.Reason != "" {
			reason := effective.Reason
			info.EffectiveReason = &reason
		}

		response.Servers = append(response.Servers, info)
	}

	// Populate slot_groups from daemon config (additive — does not alter servers).
	response.SlotGroups = s.buildSlotGroups()

	// Serialize to JSON
	jsonBytes, err := json.Marshal(response)
	if err != nil {
		return nil, fmt.Errorf("failed to serialize response: %w", err)
	}

	return &ToolCallResult{
		Content: []ToolContent{
			{Type: "text", Text: string(jsonBytes)},
		},
	}, nil
}

func (s *Server) lifecycleSnapshot(name string) *SessionLifecycleSnapshot {
	if s.sessionLifecycleAccessor == nil {
		return nil
	}
	return s.sessionLifecycleAccessor.SessionLifecycleSnapshot(name)
}

func lifecycleBackendState(snapshot *SessionLifecycleSnapshot) string {
	if snapshot == nil {
		return ""
	}
	return snapshot.BackendState
}

func (s *Server) codemodeNamespace(name string) string {
	if s.catalog != nil {
		if entry := s.catalog.Get(name); entry != nil {
			return entry.GetCodemodeNamespace()
		}
	}
	return name
}

// buildSlotGroups produces the slot_groups section for vision_list. It reads
// the declarative SlotGroups from the daemon config and resolves each group's
// slot membership by scanning cfg.Servers for entries whose SlotGroup field
// matches the group name.
func (s *Server) buildSlotGroups() []SlotGroupEntry {
	if s.daemonConfig == nil || len(s.daemonConfig.SlotGroups) == 0 {
		return []SlotGroupEntry{}
	}

	groups := make([]SlotGroupEntry, 0, len(s.daemonConfig.SlotGroups))
	for name, sg := range s.daemonConfig.SlotGroups {
		entry := SlotGroupEntry{
			Name:      name,
			GroupPort: sg.GroupPort,
		}

		// Collect slot members from the expanded servers map.
		for srvName, srvCfg := range s.daemonConfig.Servers {
			if srvCfg != nil && srvCfg.SlotGroup == name {
				entry.Slots = append(entry.Slots, srvName)
			}
		}
		sort.Strings(entry.Slots)

		groups = append(groups, entry)
	}

	// Sort groups by name for deterministic output.
	slices.SortFunc(groups, func(a, b SlotGroupEntry) int {
		return cmp.Compare(a.Name, b.Name)
	})

	return groups
}

// SlotSessionAccessor provides active session counts for slot servers.
// The daemon wires a concrete implementation; when nil, active_sessions is 0.
type SlotSessionAccessor interface {
	ActiveSessionCount(slotName string) int
}

// ServerMetricsAccessor provides per-server session metrics.
// The daemon wires a concrete implementation; when nil, metrics are omitted.
type ServerMetricsAccessor interface {
	ServerMetricsSnapshot(serverName string) *metrics.ServerMetricsSnapshot
}

// SlotStatusResponse is the response for vision_slot_status.
type SlotStatusResponse struct {
	Groups []SlotGroupStatus `json:"groups"`
}

// SlotGroupStatus describes one slot group with per-slot session detail.
type SlotGroupStatus struct {
	GroupName string       `json:"group_name"`
	SlotCount int          `json:"slot_count"`
	Slots     []SlotDetail `json:"slots"`
}

// SlotDetail describes one slot within a group.
type SlotDetail struct {
	Name            string  `json:"name"`
	Port            int     `json:"port"`
	ActiveSessions  int     `json:"active_sessions"`
	MaxSessions     int     `json:"max_sessions"`
	EffectiveStatus string  `json:"effective_status"`
	EffectiveReason *string `json:"effective_reason,omitempty"`
	ReachabilityDetails
}

// toolSlotStatus implements vision_slot_status.
func (s *Server) toolSlotStatus(_ context.Context, _ json.RawMessage) (*ToolCallResult, error) {
	return jsonToolResult(SlotStatusResponse{Groups: s.buildSlotGroupStatuses()})
}

// AddResponse is the response for vision_add.
type AddResponse struct {
	Success         bool    `json:"success"`
	Name            string  `json:"name"`
	Status          string  `json:"status,omitempty"`
	EffectiveReason *string `json:"effective_reason,omitempty"`
	Port            *int    `json:"port,omitempty"`
	Error           *string `json:"error,omitempty"`
	ReachabilityDetails
}

// toolAdd implements vision_add.
// Adds a server from the catalog and optionally starts it.
func (s *Server) toolAdd(ctx context.Context, args json.RawMessage) (*ToolCallResult, error) {
	var params struct {
		Name  string `json:"name"`
		Start *bool  `json:"start"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return nil, NewValidationError("invalid arguments: " + err.Error())
	}

	if params.Name == "" {
		return nil, NewValidationError("name is required")
	}

	// Default start to true
	shouldStart := true
	if params.Start != nil {
		shouldStart = *params.Start
	}

	// Check if server is already configured in runtime registry
	if s.registry != nil {
		srv := s.registry.Get(params.Name)
		if srv != nil {
			// Server already configured
			if !srv.IsRunning() && shouldStart {
				// Start the stopped server
				if err := s.registry.Start(params.Name); err != nil {
					errMsg := fmt.Sprintf("Failed to start server: %s", err.Error())
					response := AddResponse{
						Success: false,
						Name:    params.Name,
						Error:   &errMsg,
					}
					return jsonToolResult(response)
				}
				// Refresh status
				srv = s.registry.Get(params.Name)
			}

			if srv != nil {
				status := srv.Status()
				effective := deriveEffectiveStatus(status.State, lifecycleBackendState(s.lifecycleSnapshot(status.Name)), s.reachabilityFor(status.Name), status.Transport.IsReachabilityProbeable(), status.Uptime, s.reachabilityGrace, status.LastError)
				port := status.Port
				response := AddResponse{
					Success:             true,
					Name:                params.Name,
					Status:              effective.Status,
					Port:                &port,
					ReachabilityDetails: effective.Reachability,
				}
				if effective.Reason != "" {
					reason := effective.Reason
					response.EffectiveReason = &reason
				}
				if status.LastError != "" {
					errorText := scrubSecrets(status.LastError)
					response.Error = &errorText
				}
				return jsonToolResult(response)
			}

			// Shouldn't happen, but handle it
			errMsg := fmt.Sprintf("Server '%s' is already configured", params.Name)
			response := AddResponse{
				Success: false,
				Name:    params.Name,
				Error:   &errMsg,
			}
			return jsonToolResult(response)
		}
	}

	// Check if server exists in catalog
	if s.catalog == nil {
		errMsg := fmt.Sprintf("Server '%s' not found in registry", params.Name)
		response := AddResponse{
			Success: false,
			Name:    params.Name,
			Error:   &errMsg,
		}
		return jsonToolResult(response)
	}

	entry := s.catalog.Get(params.Name)
	if entry == nil {
		errMsg := fmt.Sprintf("Server '%s' not found in registry", params.Name)
		response := AddResponse{
			Success: false,
			Name:    params.Name,
			Error:   &errMsg,
		}
		return jsonToolResult(response)
	}

	// Auto-add catalog server to runtime registry and persist to config.
	serverCfg, err := s.catalogEntryToServerConfig(entry)
	if err != nil {
		errMsg := fmt.Sprintf("Failed to configure server: %s", err.Error())
		response := AddResponse{
			Success: false,
			Name:    params.Name,
			Error:   &errMsg,
		}
		return jsonToolResult(response)
	}

	// Add to runtime registry.
	if err := s.registry.Add(params.Name, serverCfg); err != nil {
		errMsg := fmt.Sprintf("Failed to add server to registry: %s", err.Error())
		response := AddResponse{
			Success: false,
			Name:    params.Name,
			Error:   &errMsg,
		}
		return jsonToolResult(response)
	}

	// Persist to config file (best-effort — server is already in runtime registry).
	if s.daemonConfig != nil {
		if s.daemonConfig.Servers == nil {
			s.daemonConfig.Servers = make(map[string]*config.ServerConfig)
		}
		s.daemonConfig.Servers[params.Name] = serverCfg
		if s.configPath != "" {
			if err := config.Save(s.daemonConfig, s.configPath); err != nil {
				s.logger.Warn("failed to persist config after adding server",
					slog.String("server", params.Name),
					slog.String("error", err.Error()),
				)
			}
		}
	}
	// Start the server if requested (triggers Streamable HTTP proxy setup via EventServerStarted).
	if shouldStart {
		if err := s.registry.Start(params.Name); err != nil {
			errMsg := fmt.Sprintf("Server added but failed to start: %s", err.Error())
			port := serverCfg.Port
			response := AddResponse{
				Success: false,
				Name:    params.Name,
				Port:    &port,
				Error:   &errMsg,
			}
			return jsonToolResult(response)
		}
	}

	srv := s.registry.Get(params.Name)
	if srv != nil {
		status := srv.Status()
		effective := deriveEffectiveStatus(status.State, lifecycleBackendState(s.lifecycleSnapshot(status.Name)), s.reachabilityFor(status.Name), status.Transport.IsReachabilityProbeable(), status.Uptime, s.reachabilityGrace, status.LastError)
		port := status.Port
		response := AddResponse{
			Success:             true,
			Name:                params.Name,
			Status:              effective.Status,
			Port:                &port,
			ReachabilityDetails: effective.Reachability,
		}
		if effective.Reason != "" {
			reason := effective.Reason
			response.EffectiveReason = &reason
		}
		return jsonToolResult(response)
	}

	port := serverCfg.Port
	response := AddResponse{
		Success: true,
		Name:    params.Name,
		Status:  "stopped",
		Port:    &port,
	}
	return jsonToolResult(response)
}

// jsonToolResult creates a ToolCallResult with JSON content.
func jsonToolResult(v interface{}) (*ToolCallResult, error) {
	jsonBytes, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("failed to serialize response: %w", err)
	}
	return &ToolCallResult{
		Content: []ToolContent{
			{Type: "text", Text: string(jsonBytes)},
		},
	}, nil
}

// RemoveResponse is the response for vision_remove.
type RemoveResponse struct {
	Success bool    `json:"success"`
	Name    string  `json:"name"`
	Error   *string `json:"error,omitempty"`
}

// toolRemove implements vision_remove.
func (s *Server) toolRemove(ctx context.Context, args json.RawMessage) (*ToolCallResult, error) {
	var params struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return nil, NewValidationError("invalid arguments: " + err.Error())
	}

	if params.Name == "" {
		return nil, NewValidationError("name is required")
	}

	if s.registry == nil {
		errMsg := "Registry not initialized"
		response := RemoveResponse{
			Success: false,
			Name:    params.Name,
			Error:   &errMsg,
		}
		return jsonToolResult(response)
	}

	// Get server first to check existence
	srv := s.registry.Get(params.Name)
	if srv == nil {
		errMsg := fmt.Sprintf("Server '%s' is not configured", params.Name)
		response := RemoveResponse{
			Success: false,
			Name:    params.Name,
			Error:   &errMsg,
		}
		return jsonToolResult(response)
	}

	// Stop if running
	if srv.IsRunning() {
		if err := s.registry.Stop(params.Name); err != nil {
			errMsg := fmt.Sprintf("Failed to stop server: %s", err.Error())
			response := RemoveResponse{
				Success: false,
				Name:    params.Name,
				Error:   &errMsg,
			}
			return jsonToolResult(response)
		}
	}

	// Remove from registry
	if err := s.registry.Remove(params.Name); err != nil {
		errMsg := fmt.Sprintf("Failed to remove server: %s", err.Error())
		response := RemoveResponse{
			Success: false,
			Name:    params.Name,
			Error:   &errMsg,
		}
		return jsonToolResult(response)
	}

	// Persist removal to config file (best-effort — server is already removed from runtime registry).
	if s.daemonConfig != nil && s.daemonConfig.Servers != nil {
		delete(s.daemonConfig.Servers, params.Name)
		if s.configPath != "" {
			if err := config.Save(s.daemonConfig, s.configPath); err != nil {
				s.logger.Warn("failed to persist config after removing server",
					slog.String("server", params.Name),
					slog.String("error", err.Error()),
				)
			}
		}
	}
	response := RemoveResponse{
		Success: true,
		Name:    params.Name,
	}
	return jsonToolResult(response)
}

// RestartResponse is the response for vision_restart.
type RestartResponse struct {
	Success         bool    `json:"success"`
	Name            string  `json:"name"`
	Status          string  `json:"status,omitempty"`
	EffectiveReason *string `json:"effective_reason,omitempty"`
	ProcessState    string  `json:"process_state,omitempty"`
	Port            *int    `json:"port,omitempty"`
	Error           *string `json:"error,omitempty"`
	ReachabilityDetails
}

// toolRestart implements vision_restart.
// Restarts a server that is already configured in the runtime registry, preserving
// its port assignment. Unlike vision_remove + vision_add, this never re-allocates
// the port, so servers defined in servers.yaml keep their fixed port after restart.
func (s *Server) toolRestart(ctx context.Context, args json.RawMessage) (*ToolCallResult, error) {
	var params struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return nil, NewValidationError("invalid arguments: " + err.Error())
	}

	if params.Name == "" {
		return nil, NewValidationError("name is required")
	}

	if s.registry == nil {
		errMsg := "Registry not initialized"
		errMsg = scrubSecrets(errMsg)
		return jsonToolResult(RestartResponse{Success: false, Name: params.Name, Error: &errMsg})
	}

	srv := s.registry.Get(params.Name)
	if srv == nil {
		errMsg := fmt.Sprintf("Server '%s' is not configured", params.Name)
		errMsg = scrubSecrets(errMsg)
		return jsonToolResult(RestartResponse{Success: false, Name: params.Name, Error: &errMsg})
	}

	if err := s.registry.Restart(params.Name); err != nil {
		errMsg := fmt.Sprintf("Failed to restart server: %s", err.Error())
		errMsg = scrubSecrets(errMsg)
		return jsonToolResult(RestartResponse{Success: false, Name: params.Name, Error: &errMsg})
	}

	srv = s.registry.Get(params.Name)
	if srv != nil {
		status := srv.Status()
		lifecycle := s.lifecycleSnapshot(status.Name)
		effective := deriveEffectiveStatus(status.State, lifecycleBackendState(lifecycle), s.reachabilityFor(status.Name), status.Transport.IsReachabilityProbeable(), status.Uptime, s.reachabilityGrace, status.LastError)
		port := status.Port
		response := RestartResponse{
			Success:             true,
			Name:                params.Name,
			Status:              effective.Status,
			ProcessState:        string(status.State),
			Port:                &port,
			ReachabilityDetails: effective.Reachability,
		}
		if effective.Reason != "" {
			reason := effective.Reason
			response.EffectiveReason = &reason
		}
		if status.LastError != "" {
			errorText := scrubSecrets(status.LastError)
			response.Error = &errorText
		}
		return jsonToolResult(response)
	}

	return jsonToolResult(RestartResponse{Success: true, Name: params.Name, Status: "stopped"})
}

// SearchResultEntry represents a server in search results.
type SearchResultEntry struct {
	Name              string   `json:"name"`
	CodemodeNamespace string   `json:"codemode_namespace"`
	Description       string   `json:"description"`
	Capabilities      []string `json:"capabilities"`
	Installed         bool     `json:"installed"`
}

// SearchResponse is the response for vision_search.
type SearchResponse struct {
	Success bool                `json:"success"`
	Results []SearchResultEntry `json:"results"`
	Error   *string             `json:"error,omitempty"`
}

// toolSearch implements vision_search.
// Searches the catalog for available MCP servers by name, description, or capability.
func (s *Server) toolSearch(ctx context.Context, args json.RawMessage) (*ToolCallResult, error) {
	if s.catalog == nil {
		return nil, fmt.Errorf("catalog not initialized")
	}

	var params struct {
		Query      string `json:"query"`
		Capability string `json:"capability"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return nil, NewValidationError("invalid arguments: " + err.Error())
	}

	// Search the catalog
	results := s.catalog.Search(params.Query, params.Capability)

	// Build response
	response := SearchResponse{
		Success: true,
		Results: make([]SearchResultEntry, 0, len(results)),
	}

	for _, entry := range results {
		// Check if server is configured in the registry
		installed := false
		if s.registry != nil {
			srv := s.registry.Get(entry.Name)
			installed = srv != nil
		}

		response.Results = append(response.Results, SearchResultEntry{
			Name:              entry.Name,
			CodemodeNamespace: entry.GetCodemodeNamespace(),
			Description:       entry.Description,
			Capabilities:      entry.Capabilities,
			Installed:         installed,
		})
	}

	return jsonToolResult(response)
}

// InitResponse is the response for vision_init.
type InitResponse struct {
	Success           bool     `json:"success"`
	Path              string   `json:"path"`
	Servers           []string `json:"servers"`
	ReconciledServers []string `json:"reconciled_servers,omitempty"`
	BackedUp          bool     `json:"backed_up,omitempty"`
	BackupPath        string   `json:"backup_path,omitempty"`
	Error             *string  `json:"error,omitempty"`
}

// toolInit implements vision_init.
func (s *Server) toolInit(ctx context.Context, args json.RawMessage) (*ToolCallResult, error) {
	var params struct {
		Path    string          `json:"path"`
		Servers json.RawMessage `json:"servers"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return nil, NewValidationError("invalid arguments: " + err.Error())
	}

	if params.Path == "" {
		return nil, NewValidationError("path is required")
	}
	if !filepath.IsAbs(params.Path) {
		return nil, NewValidationError("path must be absolute")
	}

	if s.registry == nil {
		return nil, fmt.Errorf("registry not initialized")
	}

	// The public MCP schema advertises a comma-separated string. Accept the
	// historical array form too so existing direct clients remain compatible.
	var requestedServers []string
	if len(params.Servers) > 0 && string(params.Servers) != "null" {
		if err := json.Unmarshal(params.Servers, &requestedServers); err != nil {
			var serverList string
			if stringErr := json.Unmarshal(params.Servers, &serverList); stringErr != nil {
				return nil, NewValidationError("servers must be a comma-separated string or string array")
			}
			requestedServers = strings.Split(serverList, ",")
		}
	}

	// Build server filter from the accepted forms.
	var serverFilter map[string]bool
	if len(requestedServers) > 0 {
		serverFilter = make(map[string]bool)
		for _, name := range requestedServers {
			name = strings.TrimSpace(name)
			if name != "" {
				serverFilter[name] = true
			}
		}
	}

	// Get running servers
	servers := s.registry.List()
	var running []*server.ManagedServer
	for _, srv := range servers {
		if srv.IsRunning() {
			// Apply filter if specified
			if serverFilter != nil && !serverFilter[srv.Name] {
				continue
			}
			running = append(running, srv)
		}
	}

	if len(running) == 0 {
		var errMsg string
		if serverFilter != nil {
			errMsg = "No running servers matching filter"
		} else {
			errMsg = "No running servers to generate config for"
		}
		response := InitResponse{
			Success: false,
			Path:    params.Path,
			Error:   &errMsg,
		}
		return jsonToolResult(response)
	}

	serverNames := make([]string, 0, len(running))
	for _, srv := range running {
		serverNames = append(serverNames, srv.Name)
	}

	existingValue, existingBytes, existed, err := loadExistingClientConfig(params.Path)
	if err != nil {
		errMsg := fmt.Sprintf("Failed to read existing config: %s", err.Error())
		response := InitResponse{
			Success: false,
			Path:    params.Path,
			Servers: serverNames,
			Error:   &errMsg,
		}
		return jsonToolResult(response)
	}

	generatedEntries := make(map[string]map[string]any, len(running))
	for _, srv := range running {
		status := srv.Status()
		generatedEntries[srv.Name] = buildClientConfigEntry(status.Port)
	}
	var existingConfig map[string]any
	if existed {
		existingConfig, err = semanticClientConfig(existingValue)
		if err != nil {
			errMsg := fmt.Sprintf("Failed to inspect existing config: %s", err.Error())
			return jsonToolResult(InitResponse{Success: false, Path: params.Path, Servers: serverNames, Error: &errMsg})
		}
	}
	reconciledServers := findReconciledServers(existingConfig, generatedEntries)

	configJSON, err := buildClientConfigBytes(existingValue, existed, generatedEntries)
	if err != nil {
		errMsg := fmt.Sprintf("Failed to generate config: %s", err.Error())
		return jsonToolResult(InitResponse{Success: false, Path: params.Path, Servers: serverNames, Error: &errMsg})
	}
	if _, err := hujson.Parse(configJSON); err != nil {
		errMsg := fmt.Sprintf("Generated config failed validation: %s", err.Error())
		return jsonToolResult(InitResponse{Success: false, Path: params.Path, Servers: serverNames, Error: &errMsg})
	}

	response := InitResponse{
		Success:           true,
		Path:              params.Path,
		Servers:           serverNames,
		ReconciledServers: reconciledServers,
	}

	// Check if file exists and create backup
	if existed {
		backupPath := params.Path + ".backup"
		if err := os.WriteFile(backupPath, existingBytes, 0600); err != nil {
			errMsg := fmt.Sprintf("Failed to create backup: %s", err.Error())
			response.Success = false
			response.Error = &errMsg
			return jsonToolResult(response)
		}
		response.BackedUp = true
		response.BackupPath = backupPath
	}

	// Ensure parent directory exists before atomic write.
	if err := os.MkdirAll(filepath.Dir(params.Path), 0755); err != nil {
		errMsg := fmt.Sprintf("Failed to create config directory: %s", err.Error())
		response.Success = false
		response.Error = &errMsg
		return jsonToolResult(response)
	}

	// Write the new config file atomically.
	if err := writeJSONFileAtomic(params.Path, configJSON); err != nil {
		errMsg := fmt.Sprintf("Permission denied: %s", err.Error())
		response.Success = false
		response.Error = &errMsg
		// Try to restore backup if we made one
		if response.BackedUp {
			if backupBytes, readErr := os.ReadFile(response.BackupPath); readErr == nil {
				_ = os.WriteFile(params.Path, backupBytes, 0600)
			}
			response.BackedUp = false
			response.BackupPath = ""
		}
		return jsonToolResult(response)
	}

	return jsonToolResult(response)
}

func loadExistingClientConfig(path string) (hujson.Value, []byte, bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return hujson.Value{}, nil, false, nil
		}
		return hujson.Value{}, nil, false, err
	}
	value, err := hujson.Parse(data)
	if err != nil {
		return hujson.Value{}, data, true, err
	}
	if _, ok := value.Value.(*hujson.Object); !ok {
		return hujson.Value{}, data, true, fmt.Errorf("root value must be an object")
	}
	return value, data, true, nil
}

func buildClientConfigEntry(port int) map[string]any {
	url := fmt.Sprintf("http://localhost:%d/mcp", port)
	return map[string]any{
		"type":    "remote",
		"url":     url,
		"enabled": true,
	}
}

func buildClientConfigBytes(existing hujson.Value, existed bool, generated map[string]map[string]any) ([]byte, error) {
	if !existed {
		config := make(map[string]any)
		mergeClientConfig(config, generated)
		return json.MarshalIndent(config, "", "  ")
	}

	updated := existing.Clone()
	restoreTrailingCommaState(existing, &updated)
	root, ok := updated.Value.(*hujson.Object)
	if !ok {
		return nil, fmt.Errorf("root value must be an object")
	}
	mcpValue := findObjectMember(root, "mcp")
	if mcpValue == nil {
		mcpObject := &hujson.Object{}
		if err := mergeHuJSONMembers(mcpObject, generated); err != nil {
			return nil, err
		}
		insertObjectMember(root, "mcp", hujson.Value{Value: mcpObject})
	} else {
		mcpObject, ok := mcpValue.Value.Value.(*hujson.Object)
		if !ok {
			return nil, fmt.Errorf("mcp value must be an object")
		}
		for _, name := range sortedGeneratedNames(generated) {
			entry := generated[name]
			member := findObjectMember(mcpObject, name)
			entryValue, err := hujsonValue(entry)
			if err != nil {
				return nil, err
			}
			if member == nil {
				insertObjectMember(mcpObject, name, entryValue)
			} else {
				member.Value.Value = entryValue.Value
			}
		}
	}
	return updated.Pack(), nil
}

func hujsonValue(entry map[string]any) (hujson.Value, error) {
	data, err := json.MarshalIndent(entry, "", "  ")
	if err != nil {
		return hujson.Value{}, err
	}
	return hujson.Parse(data)
}

func findObjectMember(object *hujson.Object, name string) *hujson.ObjectMember {
	for i := range object.Members {
		if literal, ok := object.Members[i].Name.Value.(hujson.Literal); ok && literal.String() == name {
			return &object.Members[i]
		}
	}
	return nil
}

func insertObjectMember(object *hujson.Object, name string, value hujson.Value) {
	before := memberIndent(object)
	if len(object.Members) == 0 {
		// For an empty object, AfterExtra is the exact whitespace/comments
		// between the opening brace and closing brace. Move it to the first
		// member so it is emitted once, before the member name.
		before = object.AfterExtra
		object.AfterExtra = nil
	}
	trailingComma := len(object.Members) > 0 && object.Members[len(object.Members)-1].Value.AfterExtra != nil
	if len(object.Members) > 0 {
		// Appending always turns the previous last value into a comma-bearing
		// member. Preserve the old final comma state on the new last value.
		if object.Members[len(object.Members)-1].Value.AfterExtra == nil {
			object.Members[len(object.Members)-1].Value.AfterExtra = hujson.Extra{}
		}
	}
	if trailingComma {
		value.AfterExtra = hujson.Extra{}
	}
	object.Members = append(object.Members, hujson.ObjectMember{
		Name:  hujson.Value{BeforeExtra: before, Value: hujson.String(name)},
		Value: value,
	})
}

// HuJSON represents a trailing comma as a non-nil AfterExtra on the last
// member. Clone preserves the bytes but an empty Extra loses that nil/non-nil
// distinction, so restore the structural invariant before mutating the clone.
func restoreTrailingCommaState(original hujson.Value, cloned *hujson.Value) {
	switch originalValue := original.Value.(type) {
	case *hujson.Object:
		clonedObject, ok := cloned.Value.(*hujson.Object)
		if !ok {
			return
		}
		if len(originalValue.Members) > 0 && originalValue.Members[len(originalValue.Members)-1].Value.AfterExtra != nil && clonedObject.Members[len(clonedObject.Members)-1].Value.AfterExtra == nil {
			clonedObject.Members[len(clonedObject.Members)-1].Value.AfterExtra = hujson.Extra{}
		}
		for i := range originalValue.Members {
			restoreTrailingCommaState(originalValue.Members[i].Value, &clonedObject.Members[i].Value)
		}
	case *hujson.Array:
		clonedArray, ok := cloned.Value.(*hujson.Array)
		if !ok {
			return
		}
		if len(originalValue.Elements) > 0 && originalValue.Elements[len(originalValue.Elements)-1].AfterExtra != nil && clonedArray.Elements[len(clonedArray.Elements)-1].AfterExtra == nil {
			clonedArray.Elements[len(clonedArray.Elements)-1].AfterExtra = hujson.Extra{}
		}
		for i := range originalValue.Elements {
			restoreTrailingCommaState(originalValue.Elements[i], &clonedArray.Elements[i])
		}
	}
}

func memberIndent(object *hujson.Object) hujson.Extra {
	extra := object.AfterExtra
	if len(object.Members) > 0 {
		extra = object.Members[len(object.Members)-1].Name.BeforeExtra
	}
	if i := bytes.LastIndexByte(extra, '\n'); i >= 0 {
		return append(hujson.Extra(nil), extra[i:]...)
	}
	if len(extra) > 0 {
		return hujson.Extra(" ")
	}
	return nil
}

func mergeHuJSONMembers(object *hujson.Object, generated map[string]map[string]any) error {
	for _, name := range sortedGeneratedNames(generated) {
		entry := generated[name]
		value, err := hujsonValue(entry)
		if err != nil {
			return err
		}
		insertObjectMember(object, name, value)
	}
	return nil
}

func sortedGeneratedNames(generated map[string]map[string]any) []string {
	names := make([]string, 0, len(generated))
	for name := range generated {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

func semanticClientConfig(value hujson.Value) (map[string]any, error) {
	standard := value.Clone()
	standard.Standardize()
	var config map[string]any
	if err := json.Unmarshal(standard.Pack(), &config); err != nil {
		return nil, err
	}
	if config == nil {
		config = make(map[string]any)
	}
	return config, nil
}

func mergeClientConfig(config map[string]any, generated map[string]map[string]any) {
	current, _ := config["mcp"].(map[string]any)
	if current == nil {
		current = make(map[string]any)
	}
	for name, entry := range generated {
		current[name] = entry
	}
	config["mcp"] = current
}

func findReconciledServers(existing map[string]any, generated map[string]map[string]any) []string {
	if len(generated) == 0 {
		return nil
	}
	current, _ := existing["mcp"].(map[string]any)
	var reconciled []string
	for name, entry := range generated {
		if current == nil {
			reconciled = append(reconciled, name)
			continue
		}
		existingEntry, _ := current[name].(map[string]any)
		if !reflect.DeepEqual(existingEntry, map[string]any(entry)) {
			reconciled = append(reconciled, name)
		}
	}
	return reconciled
}

func writeJSONFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	return os.Rename(tmpPath, path)
}

// StatusServers is the server counts in StatusResponse.
type StatusServers struct {
	Running int `json:"running"`
	Stopped int `json:"stopped"`
	Error   int `json:"error"`
}

// StatusResponse is the response for vision_status.
type StatusResponse struct {
	Healthy  bool          `json:"healthy"`
	Uptime   string        `json:"uptime"`
	Servers  StatusServers `json:"servers"`
	MemoryMB float64       `json:"memory_mb"`
	Warnings []string      `json:"warnings,omitempty"`
}

// toolStatus implements vision_status.
func (s *Server) toolStatus(ctx context.Context, args json.RawMessage) (*ToolCallResult, error) {
	s.mu.RLock()
	uptime := time.Since(s.startedAt)
	s.mu.RUnlock()

	var memStats runtime.MemStats
	runtime.ReadMemStats(&memStats)

	// Get registry status
	var registryStatus server.RegistryStatus
	if s.registry != nil {
		registryStatus = s.registry.Status()
	}

	// A live process is not sufficient evidence of health. Keep this aggregate
	// aligned with the per-server precedence table so unprobed and probing
	// servers cannot be reported healthy by vision_status.
	healthy := true
	if s.registry != nil {
		for _, srv := range s.registry.List() {
			status := srv.Status()
			effective := deriveEffectiveStatus(status.State, lifecycleBackendState(s.lifecycleSnapshot(status.Name)), s.reachabilityFor(status.Name), status.Transport.IsReachabilityProbeable(), status.Uptime, s.reachabilityGrace, status.LastError)
			if effective.Status != "running" {
				healthy = false
				break
			}
		}
	}

	response := StatusResponse{
		Healthy: healthy,
		Uptime:  uptime.Round(time.Second).String(),
		Servers: StatusServers{
			Running: registryStatus.RunningServers,
			Stopped: registryStatus.StoppedServers,
			Error:   registryStatus.FailedServers,
		},
		MemoryMB: float64(memStats.Alloc) / 1024 / 1024,
	}

	// Warn if bearer_token is not configured
	if s.daemonConfig != nil {
		if warning := visionmcp.BearerAuthWarning(s.daemonConfig.Security.BearerToken, s.listenerExposure); warning != "" {
			response.Warnings = append(response.Warnings, warning)
		}
	}

	return jsonToolResult(response)
}

// MetricsResponse is the response for vision_metrics.
type MetricsResponse struct {
	SessionsActive     int64 `json:"sessions_active"`
	ToolCallsTotal     int64 `json:"tool_calls_total"`
	ErrorsTotal        int64 `json:"errors_total"`
	SubprocessesActive int64 `json:"subprocesses_active"`
}

// toolMetrics implements vision_metrics.
func (s *Server) toolMetrics(_ context.Context, _ json.RawMessage) (*ToolCallResult, error) {
	var snap metrics.Snapshot
	if s.Metrics != nil {
		snap = s.Metrics.Snapshot()
	}

	return jsonToolResult(MetricsResponse{
		SessionsActive:     snap.SessionsActive,
		ToolCallsTotal:     snap.ToolCallsTotal,
		ErrorsTotal:        snap.ErrorsTotal,
		SubprocessesActive: snap.SubprocessesActive,
	})
}

// --- Guidance Tool ---

// GuidanceEntry represents guidance for a single server or tool.
type GuidanceEntry struct {
	Name           string   `json:"name"`
	NamespacedName string   `json:"namespaced_name,omitempty"`
	Priority       string   `json:"priority,omitempty"`
	Guidance       string   `json:"guidance,omitempty"`
	PreferFor      []string `json:"prefer_for,omitempty"`
	AvoidFor       []string `json:"avoid_for,omitempty"`
	Examples       []string `json:"examples,omitempty"`
}

// GuidanceResponse is the response for vision_guidance.
type GuidanceResponse struct {
	Success        bool            `json:"success"`
	GlobalGuidance string          `json:"global_guidance,omitempty"`
	Servers        []GuidanceEntry `json:"servers,omitempty"`
	Tools          []GuidanceEntry `json:"tools,omitempty"`
	Context        string          `json:"context,omitempty"`
	Error          *string         `json:"error,omitempty"`
}

// toolGuidance implements vision_guidance.
// Returns tool selection guidance and priorities for MCP servers.
func (s *Server) toolGuidance(ctx context.Context, args json.RawMessage) (*ToolCallResult, error) {
	var params struct {
		Context string `json:"context"`
		Server  string `json:"server"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return nil, NewValidationError("invalid arguments: " + err.Error())
	}

	response := GuidanceResponse{
		Success: true,
		Context: params.Context,
	}

	// Check if instructions are available
	if s.instructions == nil || !s.instructions.HasInstructions() {
		// Return helpful message if no instructions configured
		msg := "No tool guidance configured. Create ~/.config/vision/instructions.yaml to provide tool selection hints."
		response.GlobalGuidance = msg
		return jsonToolResult(response)
	}

	// Add global guidance
	response.GlobalGuidance = s.instructions.GlobalGuidance

	// If specific server requested, return only that server's guidance
	if params.Server != "" {
		serverInst := s.instructions.GetServerInstructions(params.Server)
		if serverInst == nil {
			errMsg := fmt.Sprintf("No guidance found for server '%s'", params.Server)
			response.Error = &errMsg
			return jsonToolResult(response)
		}
		response.Servers = []GuidanceEntry{
			serverInstructionsToEntry(params.Server, serverInst),
		}
		return jsonToolResult(response)
	}

	// Filter by context if provided
	contextLower := strings.ToLower(params.Context)

	// Collect server guidance
	for _, name := range s.instructions.ServerNames() {
		inst := s.instructions.GetServerInstructions(name)
		if inst == nil {
			continue
		}

		// If context provided, filter to relevant entries
		if contextLower != "" && !isRelevantToContext(inst, contextLower) {
			continue
		}

		response.Servers = append(response.Servers, serverInstructionsToEntry(name, inst))
	}

	// Collect tool guidance
	for _, name := range s.instructions.ToolNames() {
		inst := s.instructions.GetToolInstructions(name)
		if inst == nil {
			continue
		}

		// If context provided, filter to relevant entries
		if contextLower != "" && !isToolRelevantToContext(inst, contextLower) {
			continue
		}

		response.Tools = append(response.Tools, s.toolInstructionsToEntry(name, inst))
	}

	// Sort by priority (high first)
	sortGuidanceByPriority(response.Servers)
	sortGuidanceByPriority(response.Tools)

	return jsonToolResult(response)
}

// serverInstructionsToEntry converts ServerInstructions to GuidanceEntry.
func serverInstructionsToEntry(name string, inst *config.ServerInstructions) GuidanceEntry {
	return GuidanceEntry{
		Name:      name,
		Priority:  inst.Priority,
		Guidance:  inst.Guidance,
		PreferFor: inst.PreferFor,
		AvoidFor:  inst.AvoidFor,
		Examples:  inst.Examples,
	}
}

// toolInstructionsToEntry converts ToolInstructions to GuidanceEntry.
func (s *Server) toolInstructionsToEntry(name string, inst *config.ToolInstructions) GuidanceEntry {
	namespacedName := inst.NamespacedName
	if namespacedName == "" && s.instructions != nil {
		namespacedName = deriveNamespacedName(name, s.instructions.ServerNames())
	}

	return GuidanceEntry{
		Name:           name,
		NamespacedName: namespacedName,
		Priority:       inst.Priority,
		Guidance:       inst.Guidance,
		PreferFor:      inst.PreferFor,
		AvoidFor:       inst.AvoidFor,
		Examples:       inst.Examples,
	}
}

// isRelevantToContext checks if server instructions are relevant to the given context.
func isRelevantToContext(inst *config.ServerInstructions, context string) bool {
	// Check prefer_for
	for _, pref := range inst.PreferFor {
		if strings.Contains(strings.ToLower(pref), context) {
			return true
		}
	}
	// Check avoid_for (still relevant to show what NOT to use)
	for _, avoid := range inst.AvoidFor {
		if strings.Contains(strings.ToLower(avoid), context) {
			return true
		}
	}
	// Check guidance text
	if strings.Contains(strings.ToLower(inst.Guidance), context) {
		return true
	}
	return false
}

// isToolRelevantToContext checks if tool instructions are relevant to the given context.
func isToolRelevantToContext(inst *config.ToolInstructions, context string) bool {
	// Check prefer_for
	for _, pref := range inst.PreferFor {
		if strings.Contains(strings.ToLower(pref), context) {
			return true
		}
	}
	// Check avoid_for (still relevant to show what NOT to use)
	for _, avoid := range inst.AvoidFor {
		if strings.Contains(strings.ToLower(avoid), context) {
			return true
		}
	}
	// Check guidance text
	if strings.Contains(strings.ToLower(inst.Guidance), context) {
		return true
	}
	return false
}

// catalogEntryToServerConfig converts a catalog entry into a runtime ServerConfig,
// allocating the next available port from the daemon config.
func (s *Server) catalogEntryToServerConfig(entry *catalog.Entry) (*config.ServerConfig, error) {
	if s.daemonConfig == nil {
		return nil, fmt.Errorf("daemon config not available for port allocation")
	}

	port, err := s.daemonConfig.NextAvailablePort()
	if err != nil {
		return nil, fmt.Errorf("port allocation failed: %w", err)
	}

	cfg := &config.ServerConfig{
		Port:      port,
		Autostart: true,
	}

	// Set transport-specific fields from catalog entry.
	switch entry.GetTransport() {
	case "stdio":
		cfg.Transport = config.TransportStdio
		cfg.Command = entry.Command
		cfg.Args = entry.Args
	case "http":
		cfg.Transport = config.TransportHTTP
		cfg.URL = entry.URL
	case "sse":
		cfg.Transport = config.TransportSSE
		cfg.URL = entry.URL
	}

	// Build environment hints from catalog's required env vars.
	// Only set env vars that are available in the current environment.
	if len(entry.EnvVars) > 0 {
		env := make(map[string]string)
		for _, varName := range entry.EnvVars {
			if val, ok := os.LookupEnv(varName); ok && val != "" {
				env[varName] = val
			}
		}
		if len(env) > 0 {
			cfg.Env = env
		}
	}

	cfg.ApplyDefaults()
	return cfg, nil
}

// sortGuidanceByPriority sorts guidance entries by priority (high > medium > low > empty).
func sortGuidanceByPriority(entries []GuidanceEntry) {
	priorityOrder := map[string]int{
		"high":   0,
		"medium": 1,
		"low":    2,
		"":       3,
	}

	for i := 0; i < len(entries)-1; i++ {
		for j := i + 1; j < len(entries); j++ {
			pi := priorityOrder[entries[i].Priority]
			pj := priorityOrder[entries[j].Priority]
			if pi > pj || (pi == pj && entries[i].Name > entries[j].Name) {
				entries[i], entries[j] = entries[j], entries[i]
			}
		}
	}
}
