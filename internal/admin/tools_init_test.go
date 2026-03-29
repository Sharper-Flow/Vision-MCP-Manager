package admin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/jrede/vision/internal/config"
	"github.com/jrede/vision/internal/server"
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
