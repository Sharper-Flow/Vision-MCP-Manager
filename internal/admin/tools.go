package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
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

	// Check if tool exists - return JSON-RPC error for unknown tools per MCP spec
	if !s.isValidTool(params.Name) {
		return bridge.NewErrorResponse(req.ID, bridge.NewError(
			bridge.CodeMethodNotFound,
			fmt.Sprintf("Unknown tool: %s", params.Name),
		))
	}

	// Dispatch to tool handler
	result, err := s.callTool(ctx, params.Name, params.Arguments)
	if err != nil {
		// Check if this is a validation error (invalid params)
		if isValidationError(err) {
			return bridge.NewErrorResponse(req.ID, bridge.NewError(
				bridge.CodeInvalidParams,
				err.Error(),
			))
		}
		// Return other errors as tool result (business logic errors)
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

// isValidTool checks if a tool name is valid.
func (s *Server) isValidTool(name string) bool {
	switch name {
	case "vision_list", "vision_add", "vision_remove", "vision_search", "vision_init", "vision_status":
		return true
	default:
		return false
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
// Note: Tool name validation is done in handleToolsCall before this is called.
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
		// Should never reach here due to isValidTool check
		return nil, fmt.Errorf("unknown tool: %s", name)
	}
}

// --- Tool Implementations ---

// ListServerEntry represents a server in the vision_list response.
type ListServerEntry struct {
	Name   string  `json:"name"`
	Status string  `json:"status"`
	Port   *int    `json:"port"`
	PID    *int    `json:"pid"`
	Uptime *string `json:"uptime"`
	Error  *string `json:"error"`
}

// ListResponse is the response for vision_list.
type ListResponse struct {
	Servers []ListServerEntry `json:"servers"`
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
		info := ListServerEntry{
			Name:   status.Name,
			Status: mapStateToStatus(string(status.State)),
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
			errStr := status.LastError
			info.Error = &errStr
		}

		response.Servers = append(response.Servers, info)
	}

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

// mapStateToStatus maps internal state names to spec status values.
func mapStateToStatus(state string) string {
	switch state {
	case "running":
		return "running"
	case "starting":
		return "starting"
	case "stopped", "stopping":
		return "stopped"
	case "failed", "crashed":
		return "error"
	default:
		return "stopped"
	}
}

// AddResponse is the response for vision_add.
type AddResponse struct {
	Success bool    `json:"success"`
	Name    string  `json:"name"`
	Status  string  `json:"status,omitempty"`
	Port    *int    `json:"port,omitempty"`
	Error   *string `json:"error,omitempty"`
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
				port := status.Port
				response := AddResponse{
					Success: true,
					Name:    params.Name,
					Status:  mapStateToStatus(string(status.State)),
					Port:    &port,
				}
				if status.LastError != "" {
					response.Error = &status.LastError
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

	// TODO: In future, we could auto-add the server to configuration here.
	// For now, the server must be pre-configured in servers.yaml.
	// Return success=false with instructions.
	errMsg := fmt.Sprintf("Server '%s' found in catalog but not configured. Add to ~/.config/vision/servers.yaml to use.", params.Name)
	response := AddResponse{
		Success: false,
		Name:    params.Name,
		Error:   &errMsg,
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

	response := RemoveResponse{
		Success: true,
		Name:    params.Name,
	}
	return jsonToolResult(response)
}

// SearchResultEntry represents a server in search results.
type SearchResultEntry struct {
	Name         string   `json:"name"`
	Description  string   `json:"description"`
	Capabilities []string `json:"capabilities"`
	Installed    bool     `json:"installed"`
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

	// Validate: query is required per spec
	if params.Query == "" && params.Capability == "" {
		return nil, NewValidationError("query is required")
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
			Name:         entry.Name,
			Description:  entry.Description,
			Capabilities: entry.Capabilities,
			Installed:    installed,
		})
	}

	return jsonToolResult(response)
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

// InitResponse is the response for vision_init.
type InitResponse struct {
	Success    bool     `json:"success"`
	Path       string   `json:"path"`
	Servers    []string `json:"servers"`
	BackedUp   bool     `json:"backed_up,omitempty"`
	BackupPath string   `json:"backup_path,omitempty"`
	Error      *string  `json:"error,omitempty"`
}

// toolInit implements vision_init.
func (s *Server) toolInit(ctx context.Context, args json.RawMessage) (*ToolCallResult, error) {
	if s.registry == nil {
		return nil, fmt.Errorf("registry not initialized")
	}

	var params struct {
		Path    string   `json:"path"`
		Servers []string `json:"servers"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return nil, NewValidationError("invalid arguments: " + err.Error())
	}

	if params.Path == "" {
		params.Path = ".opencode.json"
	}

	// Build servers filter from array
	var serverFilter map[string]bool
	if len(params.Servers) > 0 {
		serverFilter = make(map[string]bool)
		for _, name := range params.Servers {
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

	// Build MCP servers config
	mcpServers := make(map[string]map[string]string)
	serverNames := make([]string, 0, len(running))
	for _, srv := range running {
		status := srv.Status()
		mcpServers[srv.Name] = map[string]string{
			"url": fmt.Sprintf("http://localhost:%d/mcp", status.Port),
		}
		serverNames = append(serverNames, srv.Name)
	}

	config := map[string]interface{}{
		"mcpServers": mcpServers,
	}

	configJSON, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		errMsg := fmt.Sprintf("Failed to generate config: %s", err.Error())
		response := InitResponse{
			Success: false,
			Path:    params.Path,
			Error:   &errMsg,
		}
		return jsonToolResult(response)
	}

	response := InitResponse{
		Success: true,
		Path:    params.Path,
		Servers: serverNames,
	}

	// Check if file exists and create backup
	if _, err := os.Stat(params.Path); err == nil {
		backupPath := params.Path + ".backup"
		if err := os.Rename(params.Path, backupPath); err != nil {
			errMsg := fmt.Sprintf("Failed to create backup: %s", err.Error())
			response.Success = false
			response.Error = &errMsg
			return jsonToolResult(response)
		}
		response.BackedUp = true
		response.BackupPath = backupPath
	}

	// Write the new config file
	if err := os.WriteFile(params.Path, configJSON, 0644); err != nil {
		errMsg := fmt.Sprintf("Permission denied: %s", err.Error())
		response.Success = false
		response.Error = &errMsg
		// Try to restore backup if we made one
		if response.BackedUp {
			os.Rename(response.BackupPath, params.Path)
			response.BackedUp = false
			response.BackupPath = ""
		}
		return jsonToolResult(response)
	}

	return jsonToolResult(response)
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

	// Healthy if no failed servers
	healthy := registryStatus.FailedServers == 0

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

	return jsonToolResult(response)
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
