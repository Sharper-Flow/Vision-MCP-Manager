package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime"
	"time"

	"github.com/jrede/vision/internal/bridge"
	"github.com/jrede/vision/internal/server"
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
			Description: "List all registered MCP servers with their current status (running/stopped/failed)",
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
			Description: "Generate MCP client configuration for running servers",
			InputSchema: InputSchema{
				Type: "object",
				Properties: map[string]Property{
					"path": {
						Type:        "string",
						Description: "Path to write the configuration file (default: .opencode.json)",
						Default:     ".opencode.json",
					},
					"servers": {
						Type:        "string",
						Description: "Comma-separated list of server names to include (default: all running)",
					},
				},
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
	}
}

// --- MCP Protocol Handlers ---

// InitializeResult is the response to the initialize method.
type InitializeResult struct {
	ProtocolVersion string       `json:"protocolVersion"`
	Capabilities    Capabilities `json:"capabilities"`
	ServerInfo      ServerInfo   `json:"serverInfo"`
}

// Capabilities describes the server's capabilities.
type Capabilities struct {
	Tools ToolsCapability `json:"tools"`
}

// ToolsCapability describes tool capabilities.
type ToolsCapability struct {
	ListChanged bool `json:"listChanged"`
}

// ServerInfo describes the server.
type ServerInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// handleInitialize handles the MCP initialize method.
func (s *Server) handleInitialize(req *bridge.Request) *bridge.Response {
	result := InitializeResult{
		ProtocolVersion: "2024-11-05",
		Capabilities: Capabilities{
			Tools: ToolsCapability{
				ListChanged: false,
			},
		},
		ServerInfo: ServerInfo{
			Name:    "vision-admin",
			Version: "1.0.0",
		},
	}

	resp, err := bridge.NewResponse(req.ID, result)
	if err != nil {
		return bridge.NewErrorResponse(req.ID, bridge.NewError(
			bridge.CodeInternalError,
			"Failed to create response: "+err.Error(),
		))
	}
	return resp
}

// ToolsListResult is the response to tools/list.
type ToolsListResult struct {
	Tools []Tool `json:"tools"`
}

// handleToolsList handles the tools/list method.
func (s *Server) handleToolsList(req *bridge.Request) *bridge.Response {
	result := ToolsListResult{
		Tools: s.getTools(),
	}

	resp, err := bridge.NewResponse(req.ID, result)
	if err != nil {
		return bridge.NewErrorResponse(req.ID, bridge.NewError(
			bridge.CodeInternalError,
			"Failed to create response: "+err.Error(),
		))
	}
	return resp
}

// ToolCallParams is the params for tools/call.
type ToolCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
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

// handleToolsCall handles the tools/call method.
func (s *Server) handleToolsCall(ctx context.Context, req *bridge.Request) *bridge.Response {
	var params ToolCallParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return bridge.NewErrorResponse(req.ID, bridge.NewError(
			bridge.CodeInvalidParams,
			"Invalid params: "+err.Error(),
		))
	}

	// Dispatch to tool handler
	result, err := s.callTool(ctx, params.Name, params.Arguments)
	if err != nil {
		// Return error as tool result (not JSON-RPC error)
		errorResult := ToolCallResult{
			IsError: true,
			Content: []ToolContent{
				{Type: "text", Text: err.Error()},
			},
		}
		resp, _ := bridge.NewResponse(req.ID, errorResult)
		return resp
	}

	resp, err := bridge.NewResponse(req.ID, result)
	if err != nil {
		return bridge.NewErrorResponse(req.ID, bridge.NewError(
			bridge.CodeInternalError,
			"Failed to create response: "+err.Error(),
		))
	}
	return resp
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
	case "vision_search":
		return s.toolSearch(ctx, args)
	case "vision_init":
		return s.toolInit(ctx, args)
	case "vision_status":
		return s.toolStatus(ctx, args)
	default:
		return nil, fmt.Errorf("unknown tool: %s", name)
	}
}

// --- Tool Implementations ---

// toolList implements vision_list.
func (s *Server) toolList(ctx context.Context, args json.RawMessage) (*ToolCallResult, error) {
	if s.registry == nil {
		return nil, fmt.Errorf("registry not initialized")
	}

	servers := s.registry.List()
	if len(servers) == 0 {
		return &ToolCallResult{
			Content: []ToolContent{
				{Type: "text", Text: "No servers registered"},
			},
		}, nil
	}

	// Build formatted output
	var output string
	output += fmt.Sprintf("Registered servers: %d\n\n", len(servers))

	for _, srv := range servers {
		status := srv.Status()
		icon := stateIcon(string(status.State))
		output += fmt.Sprintf("%s %s (port %d)\n", icon, status.Name, status.Port)
		output += fmt.Sprintf("   State: %s\n", status.State)
		if status.Uptime > 0 {
			output += fmt.Sprintf("   Uptime: %s\n", status.Uptime.Round(time.Second))
		}
		if status.LastError != "" {
			output += fmt.Sprintf("   Error: %s\n", status.LastError)
		}
		output += "\n"
	}

	return &ToolCallResult{
		Content: []ToolContent{
			{Type: "text", Text: output},
		},
	}, nil
}

// toolAdd implements vision_add.
func (s *Server) toolAdd(ctx context.Context, args json.RawMessage) (*ToolCallResult, error) {
	if s.registry == nil {
		return nil, fmt.Errorf("registry not initialized")
	}

	var params struct {
		Name  string `json:"name"`
		Start *bool  `json:"start"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	if params.Name == "" {
		return nil, fmt.Errorf("name is required")
	}

	// Default start to true
	shouldStart := true
	if params.Start != nil {
		shouldStart = *params.Start
	}

	// Check if server exists in registry
	srv := s.registry.Get(params.Name)
	if srv == nil {
		return nil, fmt.Errorf("server not found in registry: %s", params.Name)
	}

	// Start if requested
	if shouldStart {
		if err := s.registry.Start(params.Name); err != nil {
			return nil, fmt.Errorf("failed to start server: %w", err)
		}
	}

	// Get updated status
	srv = s.registry.Get(params.Name)
	status := srv.Status()

	output := fmt.Sprintf("Server added: %s\n", params.Name)
	output += fmt.Sprintf("Port: %d\n", status.Port)
	output += fmt.Sprintf("State: %s\n", status.State)
	if shouldStart {
		output += fmt.Sprintf("Endpoint: http://localhost:%d/mcp\n", status.Port)
	}

	return &ToolCallResult{
		Content: []ToolContent{
			{Type: "text", Text: output},
		},
	}, nil
}

// toolRemove implements vision_remove.
func (s *Server) toolRemove(ctx context.Context, args json.RawMessage) (*ToolCallResult, error) {
	if s.registry == nil {
		return nil, fmt.Errorf("registry not initialized")
	}

	var params struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	if params.Name == "" {
		return nil, fmt.Errorf("name is required")
	}

	// Get server first to check existence
	srv := s.registry.Get(params.Name)
	if srv == nil {
		return nil, fmt.Errorf("server not found: %s", params.Name)
	}

	// Stop if running
	if srv.IsRunning() {
		if err := s.registry.Stop(params.Name); err != nil {
			return nil, fmt.Errorf("failed to stop server: %w", err)
		}
	}

	// Remove from registry
	if err := s.registry.Remove(params.Name); err != nil {
		return nil, fmt.Errorf("failed to remove server: %w", err)
	}

	return &ToolCallResult{
		Content: []ToolContent{
			{Type: "text", Text: fmt.Sprintf("Server removed: %s", params.Name)},
		},
	}, nil
}

// toolSearch implements vision_search.
func (s *Server) toolSearch(ctx context.Context, args json.RawMessage) (*ToolCallResult, error) {
	if s.registry == nil {
		return nil, fmt.Errorf("registry not initialized")
	}

	var params struct {
		Query      string `json:"query"`
		Capability string `json:"capability"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	// For now, just list all servers (search will be enhanced in Phase 15)
	// TODO: Add capability-based search when registry enhancement is done
	servers := s.registry.List()

	if len(servers) == 0 {
		return &ToolCallResult{
			Content: []ToolContent{
				{Type: "text", Text: "No servers found in registry"},
			},
		}, nil
	}

	// Basic name matching if query provided
	var matches []*server.ManagedServer
	if params.Query != "" {
		for _, srv := range servers {
			if containsIgnoreCase(srv.Name, params.Query) {
				matches = append(matches, srv)
			}
		}
	} else {
		matches = servers
	}

	if len(matches) == 0 {
		return &ToolCallResult{
			Content: []ToolContent{
				{Type: "text", Text: fmt.Sprintf("No servers matching query: %s", params.Query)},
			},
		}, nil
	}

	output := fmt.Sprintf("Found %d server(s):\n\n", len(matches))
	for _, srv := range matches {
		status := srv.Status()
		installed := "not configured"
		if srv.IsRunning() {
			installed = fmt.Sprintf("running on port %d", status.Port)
		} else if status.Port > 0 {
			installed = fmt.Sprintf("configured (port %d)", status.Port)
		}
		output += fmt.Sprintf("- %s: %s\n", srv.Name, installed)
	}

	return &ToolCallResult{
		Content: []ToolContent{
			{Type: "text", Text: output},
		},
	}, nil
}

// toolInit implements vision_init.
func (s *Server) toolInit(ctx context.Context, args json.RawMessage) (*ToolCallResult, error) {
	if s.registry == nil {
		return nil, fmt.Errorf("registry not initialized")
	}

	var params struct {
		Path    string `json:"path"`
		Servers string `json:"servers"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	if params.Path == "" {
		params.Path = ".opencode.json"
	}

	// Get running servers
	servers := s.registry.List()
	var running []*server.ManagedServer
	for _, srv := range servers {
		if srv.IsRunning() {
			running = append(running, srv)
		}
	}

	if len(running) == 0 {
		return &ToolCallResult{
			Content: []ToolContent{
				{Type: "text", Text: "No running servers to generate config for"},
			},
		}, nil
	}

	// Build MCP servers config
	mcpServers := make(map[string]map[string]string)
	for _, srv := range running {
		status := srv.Status()
		mcpServers[srv.Name] = map[string]string{
			"url": fmt.Sprintf("http://localhost:%d/mcp", status.Port),
		}
	}

	config := map[string]interface{}{
		"mcpServers": mcpServers,
	}

	configJSON, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("failed to generate config: %w", err)
	}

	// Note: In production, we'd write this to the file
	// For now, just return the config content
	output := fmt.Sprintf("Generated configuration for %d server(s):\n\n", len(running))
	output += fmt.Sprintf("Path: %s\n\n", params.Path)
	output += "```json\n"
	output += string(configJSON)
	output += "\n```\n"

	return &ToolCallResult{
		Content: []ToolContent{
			{Type: "text", Text: output},
		},
	}, nil
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

	output := "Vision Daemon Status\n"
	output += "====================\n\n"
	output += fmt.Sprintf("Uptime: %s\n", uptime.Round(time.Second))
	output += fmt.Sprintf("Memory: %.1f MB (alloc) / %.1f MB (sys)\n",
		float64(memStats.Alloc)/1024/1024,
		float64(memStats.Sys)/1024/1024,
	)
	output += fmt.Sprintf("Goroutines: %d\n\n", runtime.NumGoroutine())
	output += "Servers:\n"
	output += fmt.Sprintf("  Total: %d\n", registryStatus.TotalServers)
	output += fmt.Sprintf("  Running: %d\n", registryStatus.RunningServers)
	output += fmt.Sprintf("  Stopped: %d\n", registryStatus.StoppedServers)
	output += fmt.Sprintf("  Failed: %d\n", registryStatus.FailedServers)

	return &ToolCallResult{
		Content: []ToolContent{
			{Type: "text", Text: output},
		},
	}, nil
}

// --- Helpers ---

// stateIcon returns an emoji for a server state.
func stateIcon(state string) string {
	switch state {
	case "running":
		return "[OK]"
	case "starting":
		return "[..]"
	case "stopped":
		return "[--]"
	case "stopping":
		return "[..]"
	case "failed", "crashed":
		return "[!!]"
	default:
		return "[??]"
	}
}

// containsIgnoreCase checks if haystack contains needle (case-insensitive).
func containsIgnoreCase(haystack, needle string) bool {
	return len(haystack) >= len(needle) &&
		(haystack == needle ||
			len(needle) > 0 && containsLower(toLower(haystack), toLower(needle)))
}

func containsLower(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func toLower(s string) string {
	b := make([]byte, len(s))
	for i := range s {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 32
		} else {
			b[i] = c
		}
	}
	return string(b)
}
