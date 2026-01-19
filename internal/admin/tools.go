package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime"
	"strings"
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
		icon := getStateIcon(string(status.State))
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
// If the server is already in the runtime registry, it starts it.
// If not, it looks up the server in the catalog and returns install instructions.
func (s *Server) toolAdd(ctx context.Context, args json.RawMessage) (*ToolCallResult, error) {
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

	// Check if server exists in runtime registry
	if s.registry != nil {
		srv := s.registry.Get(params.Name)
		if srv != nil {
			// Server already configured - just start it
			if shouldStart {
				if err := s.registry.Start(params.Name); err != nil {
					return nil, fmt.Errorf("failed to start server: %w", err)
				}
			}

			// Get updated status
			srv = s.registry.Get(params.Name)
			if srv == nil {
				// Server was removed between start and status check (race condition)
				return nil, fmt.Errorf("server disappeared after start: %s", params.Name)
			}
			status := srv.Status()

			output := fmt.Sprintf("Server started: %s\n", params.Name)
			output += fmt.Sprintf("Port: %d\n", status.Port)
			output += fmt.Sprintf("State: %s\n", status.State)
			if shouldStart && srv.IsRunning() {
				output += fmt.Sprintf("Endpoint: http://localhost:%d/mcp\n", status.Port)
			}

			return &ToolCallResult{
				Content: []ToolContent{
					{Type: "text", Text: output},
				},
			}, nil
		}
	}

	// Server not in runtime registry - check catalog for install instructions
	if s.catalog == nil {
		return nil, fmt.Errorf("server not found: %s (no catalog available)", params.Name)
	}

	entry := s.catalog.Get(params.Name)
	if entry == nil {
		return nil, fmt.Errorf("server not found in catalog: %s", params.Name)
	}

	// Return install instructions from catalog
	output := fmt.Sprintf("Server '%s' is not configured.\n\n", params.Name)
	output += fmt.Sprintf("**%s**\n", entry.Description)
	output += "\nTo add this server, add the following to your ~/.config/vision/servers.yaml:\n\n"
	output += "```yaml\nservers:\n"
	output += fmt.Sprintf("  %s:\n", entry.Name)
	output += "    port: <choose a port 6276-6300>\n"
	if entry.Command != "" {
		output += fmt.Sprintf("    command: %s\n", entry.Command)
		if len(entry.Args) > 0 {
			output += "    args:\n"
			for _, arg := range entry.Args {
				output += fmt.Sprintf("      - \"%s\"\n", arg)
			}
		}
	}
	if entry.URL != "" {
		output += fmt.Sprintf("    url: %s\n", entry.URL)
		output += fmt.Sprintf("    transport: %s\n", entry.GetTransport())
	}
	if len(entry.EnvVars) > 0 {
		output += "    env:\n"
		for _, env := range entry.EnvVars {
			output += fmt.Sprintf("      %s: \"${%s}\"\n", env, env)
		}
	}
	output += "    autostart: true\n"
	output += "```\n"

	if len(entry.EnvVars) > 0 {
		output += "\n**Required environment variables:**\n"
		for _, env := range entry.EnvVars {
			output += fmt.Sprintf("- `%s`\n", env)
		}
	}

	if entry.Source != "" {
		output += fmt.Sprintf("\n**Source:** %s\n", entry.Source)
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
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	// Search the catalog
	results := s.catalog.Search(params.Query, params.Capability)

	if len(results) == 0 {
		msg := "No servers found"
		if params.Query != "" {
			msg = fmt.Sprintf("No servers matching query: %s", params.Query)
		}
		if params.Capability != "" {
			msg = fmt.Sprintf("No servers with capability: %s", params.Capability)
		}
		return &ToolCallResult{
			Content: []ToolContent{
				{Type: "text", Text: msg},
			},
		}, nil
	}

	// Build output showing catalog entries with install status
	output := fmt.Sprintf("Found %d server(s):\n\n", len(results))

	for _, entry := range results {
		// Check if server is configured/running in the registry
		status := s.getServerStatus(entry.Name)

		output += fmt.Sprintf("**%s** - %s\n", entry.Name, entry.Description)
		output += fmt.Sprintf("   Status: %s\n", status)
		if len(entry.Capabilities) > 0 {
			output += fmt.Sprintf("   Capabilities: %s\n", strings.Join(entry.Capabilities, ", "))
		}
		if len(entry.EnvVars) > 0 {
			output += fmt.Sprintf("   Required env: %s\n", strings.Join(entry.EnvVars, ", "))
		}
		output += "\n"
	}

	return &ToolCallResult{
		Content: []ToolContent{
			{Type: "text", Text: output},
		},
	}, nil
}

// getServerStatus returns the status of a server (from runtime registry).
func (s *Server) getServerStatus(name string) string {
	if s.registry == nil {
		return "not configured"
	}

	srv := s.registry.Get(name)
	if srv == nil {
		return "not configured"
	}

	if srv.IsRunning() {
		status := srv.Status()
		return fmt.Sprintf("running on port %d", status.Port)
	}

	status := srv.Status()
	if status.Port > 0 {
		return fmt.Sprintf("configured (port %d, stopped)", status.Port)
	}

	return "configured (stopped)"
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

	// Parse optional servers filter (comma-separated)
	var serverFilter map[string]bool
	if params.Servers != "" {
		serverFilter = make(map[string]bool)
		for _, name := range strings.Split(params.Servers, ",") {
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
		if serverFilter != nil {
			return &ToolCallResult{
				Content: []ToolContent{
					{Type: "text", Text: "No running servers matching filter to generate config for"},
				},
			}, nil
		}
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

// getStateIcon returns a status icon for a server state.
func getStateIcon(state string) string {
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
