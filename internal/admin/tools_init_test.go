package admin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/catalog"
	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/config"
	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/server"
)

func TestToolInit_WritesOpenCodeConfig(t *testing.T) {
	registry := server.NewRegistry(nil, nil)
	if err := registry.Add("kagi", &config.ServerConfig{Port: 6279, Command: "uvx"}); err != nil {
		t.Fatalf("Add(kagi): %v", err)
	}
	registry.Get("kagi").State = server.StateRunning

	srv := &Server{registry: registry}
	path := filepath.Join(t.TempDir(), ".opencode.json")

	result, err := srv.toolInit(context.Background(), mustJSON(t, map[string]any{"path": path}))
	if err != nil {
		t.Fatalf("toolInit() error = %v", err)
	}

	var response InitResponse
	decodeToolJSON(t, result, &response)
	if !response.Success {
		t.Fatalf("toolInit() success = false, error = %v", response.Error)
	}
	if len(response.ReconciledServers) != 1 || response.ReconciledServers[0] != "kagi" {
		t.Fatalf("ReconciledServers = %#v, want [kagi]", response.ReconciledServers)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", path, err)
	}

	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("Unmarshal written config: %v", err)
	}

	mcp, ok := cfg["mcp"].(map[string]any)
	if !ok {
		t.Fatalf("config missing top-level mcp map: %#v", cfg)
	}
	kagi, ok := mcp["kagi"].(map[string]any)
	if !ok {
		t.Fatalf("config missing kagi entry: %#v", mcp)
	}
	if got := kagi["type"]; got != "remote" {
		t.Fatalf("kagi.type = %#v, want %q", got, "remote")
	}
	if got := kagi["url"]; got != "http://localhost:6279/mcp" {
		t.Fatalf("kagi.url = %#v, want %q", got, "http://localhost:6279/mcp")
	}
	if got := kagi["enabled"]; got != true {
		t.Fatalf("kagi.enabled = %#v, want true", got)
	}
}

func TestToolInit_ReconcilesExistingOpenCodeConfig(t *testing.T) {
	registry := server.NewRegistry(nil, nil)
	if err := registry.Add("kagi", &config.ServerConfig{Port: 6279, Command: "uvx"}); err != nil {
		t.Fatalf("Add(kagi): %v", err)
	}
	registry.Get("kagi").State = server.StateRunning

	srv := &Server{registry: registry}
	path := filepath.Join(t.TempDir(), "opencode.json")
	existing := map[string]any{
		"theme": "ayu-dark",
		"mcp": map[string]any{
			"kagi": map[string]any{
				"type":    "remote",
				"url":     "http://localhost:6283/mcp",
				"enabled": true,
			},
			"custom": map[string]any{
				"type":    "remote",
				"url":     "http://localhost:9999/mcp",
				"enabled": true,
			},
		},
	}
	data, err := json.Marshal(existing)
	if err != nil {
		t.Fatalf("Marshal existing config: %v", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("WriteFile(%q): %v", path, err)
	}

	result, err := srv.toolInit(context.Background(), mustJSON(t, map[string]any{"path": path, "servers": []string{"kagi"}}))
	if err != nil {
		t.Fatalf("toolInit() error = %v", err)
	}

	var response InitResponse
	decodeToolJSON(t, result, &response)
	if !response.Success {
		t.Fatalf("toolInit() success = false, error = %v", response.Error)
	}

	updatedData, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", path, err)
	}

	var cfg map[string]any
	if err := json.Unmarshal(updatedData, &cfg); err != nil {
		t.Fatalf("Unmarshal updated config: %v", err)
	}
	if got := cfg["theme"]; got != "ayu-dark" {
		t.Fatalf("theme = %#v, want %q", got, "ayu-dark")
	}

	mcp := cfg["mcp"].(map[string]any)
	kagi := mcp["kagi"].(map[string]any)
	if got := kagi["url"]; got != "http://localhost:6279/mcp" {
		t.Fatalf("kagi.url = %#v, want %q", got, "http://localhost:6279/mcp")
	}
	custom := mcp["custom"].(map[string]any)
	if got := custom["url"]; got != "http://localhost:9999/mcp" {
		t.Fatalf("custom.url = %#v, want preserved value", got)
	}
}

func TestToolSearch_EmptyQueryReturnsAlphabetical(t *testing.T) {
	c := catalog.New()
	c.Add(&catalog.Entry{Name: "zebra", Description: "Last"})
	c.Add(&catalog.Entry{Name: "alpha", Description: "First"})
	c.Add(&catalog.Entry{Name: "beta", Description: "Second"})

	srv := &Server{catalog: c}
	result, err := srv.toolSearch(context.Background(), mustJSON(t, map[string]any{"query": ""}))
	if err != nil {
		t.Fatalf("toolSearch() error = %v", err)
	}

	var response SearchResponse
	decodeToolJSON(t, result, &response)
	if !response.Success {
		t.Fatalf("toolSearch() success = false, error = %v", response.Error)
	}
	if len(response.Results) != 3 {
		t.Fatalf("len(results) = %d, want 3", len(response.Results))
	}
	got := []string{response.Results[0].Name, response.Results[1].Name, response.Results[2].Name}
	want := []string{"alpha", "beta", "zebra"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("results = %#v, want %#v", got, want)
		}
	}
}

func TestToolAddRemove_SyncsOpenCodeConfig(t *testing.T) {
	tmp := t.TempDir()
	serversPath := filepath.Join(tmp, "servers.yaml")
	opencodePath := filepath.Join(tmp, ".opencode.json")

	daemonCfg := &config.Config{Servers: make(map[string]*config.ServerConfig)}
	registry := server.NewRegistry(nil, nil)
	srv := &Server{
		registry:     registry,
		catalog:      catalog.Default(),
		daemonConfig: daemonCfg,
		configPath:   serversPath,
	}
	start := false

	addResult, err := srv.toolAdd(context.Background(), mustJSON(t, map[string]any{"name": "kagi", "start": start}))
	if err != nil {
		t.Fatalf("toolAdd() error = %v", err)
	}
	var addResp AddResponse
	decodeToolJSON(t, addResult, &addResp)
	if !addResp.Success {
		t.Fatalf("toolAdd() success = false, error = %v", addResp.Error)
	}

	data, err := os.ReadFile(opencodePath)
	if err != nil {
		t.Fatalf("ReadFile(.opencode.json) after add: %v", err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("Unmarshal .opencode.json after add: %v", err)
	}
	mcp, ok := cfg["mcp"].(map[string]any)
	if !ok {
		t.Fatalf(".opencode.json missing mcp map: %#v", cfg)
	}
	if _, ok := mcp["kagi"].(map[string]any); !ok {
		t.Fatalf(".opencode.json missing kagi after add: %#v", mcp)
	}

	removeResult, err := srv.toolRemove(context.Background(), mustJSON(t, map[string]any{"name": "kagi"}))
	if err != nil {
		t.Fatalf("toolRemove() error = %v", err)
	}
	var removeResp RemoveResponse
	decodeToolJSON(t, removeResult, &removeResp)
	if !removeResp.Success {
		t.Fatalf("toolRemove() success = false, error = %v", removeResp.Error)
	}

	data, err = os.ReadFile(opencodePath)
	if err != nil {
		t.Fatalf("ReadFile(.opencode.json) after remove: %v", err)
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("Unmarshal .opencode.json after remove: %v", err)
	}
	mcp, ok = cfg["mcp"].(map[string]any)
	if !ok {
		t.Fatalf(".opencode.json missing mcp map after remove: %#v", cfg)
	}
	if _, ok := mcp["kagi"]; ok {
		t.Fatalf(".opencode.json still contains kagi after remove: %#v", mcp)
	}
}

func TestToolStatus_WarnsWhenBearerTokenEmpty(t *testing.T) {
	srv := &Server{daemonConfig: &config.Config{Security: config.SecurityConfig{}}}
	result, err := srv.toolStatus(context.Background(), mustJSON(t, map[string]any{}))
	if err != nil {
		t.Fatalf("toolStatus() error = %v", err)
	}

	var response StatusResponse
	decodeToolJSON(t, result, &response)
	if len(response.Warnings) != 1 {
		t.Fatalf("len(warnings) = %d, want 1", len(response.Warnings))
	}
	if !strings.Contains(response.Warnings[0], "bearer_token") || !strings.Contains(response.Warnings[0], "docs/AUTH.md") {
		t.Fatalf("warning = %q, want bearer_token and docs/AUTH.md", response.Warnings[0])
	}
}

func TestToolStatus_NoWarningWhenBearerTokenConfigured(t *testing.T) {
	srv := &Server{daemonConfig: &config.Config{Security: config.SecurityConfig{BearerToken: "token"}}}
	result, err := srv.toolStatus(context.Background(), mustJSON(t, map[string]any{}))
	if err != nil {
		t.Fatalf("toolStatus() error = %v", err)
	}

	var response StatusResponse
	decodeToolJSON(t, result, &response)
	if len(response.Warnings) != 0 {
		t.Fatalf("warnings = %#v, want none", response.Warnings)
	}
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	return b
}

func decodeToolJSON(t *testing.T, result *ToolCallResult, dest any) {
	t.Helper()
	if result == nil || len(result.Content) == 0 {
		t.Fatal("tool result missing content")
	}
	if err := json.Unmarshal([]byte(result.Content[0].Text), dest); err != nil {
		t.Fatalf("json.Unmarshal tool response: %v", err)
	}
}
