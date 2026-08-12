package admin

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/catalog"
	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/config"
	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/server"
)

func TestToolInit_RequiresExplicitPath(t *testing.T) {
	registry := server.NewRegistry(nil, nil)
	if err := registry.Add("kagi", &config.ServerConfig{Port: 6279, Command: "uvx"}); err != nil {
		t.Fatalf("Add(kagi): %v", err)
	}
	registry.Get("kagi").State = server.StateRunning

	workingDir := t.TempDir()
	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd(): %v", err)
	}
	if err := os.Chdir(workingDir); err != nil {
		t.Fatalf("Chdir(%q): %v", workingDir, err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(oldDir); err != nil {
			t.Errorf("restore working directory: %v", err)
		}
	})

	srv := &Server{registry: registry}
	_, err = srv.toolInit(context.Background(), mustJSON(t, map[string]any{}))
	var validationErr *ValidationError
	if !errors.As(err, &validationErr) {
		t.Fatalf("toolInit() error = %v, want validation error", err)
	}

	for _, name := range []string{".opencode.json", "opencode.json", "opencode.jsonc"} {
		if _, err := os.Stat(name); !os.IsNotExist(err) {
			t.Fatalf("unexpected guessed config %q: stat error = %v", name, err)
		}
	}
}

func TestToolInit_WritesOpenCodeJSONCConfig(t *testing.T) {
	registry := server.NewRegistry(nil, nil)
	if err := registry.Add("kagi", &config.ServerConfig{Port: 6279, Command: "uvx"}); err != nil {
		t.Fatalf("Add(kagi): %v", err)
	}
	registry.Get("kagi").State = server.StateRunning

	srv := &Server{registry: registry}
	path := filepath.Join(t.TempDir(), "opencode.jsonc")

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

func TestToolInit_AcceptsAdvertisedCommaSeparatedServerFilter(t *testing.T) {
	registry := server.NewRegistry(nil, nil)
	if err := registry.Add("kagi", &config.ServerConfig{Port: 6279, Command: "uvx"}); err != nil {
		t.Fatalf("Add(kagi): %v", err)
	}
	registry.Get("kagi").State = server.StateRunning

	srv := &Server{registry: registry}
	path := filepath.Join(t.TempDir(), "opencode.jsonc")
	result, err := srv.toolInit(context.Background(), mustJSON(t, map[string]any{
		"path":    path,
		"servers": "kagi",
	}))
	if err != nil {
		t.Fatalf("toolInit() error = %v", err)
	}

	var response InitResponse
	decodeToolJSON(t, result, &response)
	if !response.Success {
		t.Fatalf("toolInit() success = false, error = %v", response.Error)
	}
	if len(response.Servers) != 1 || response.Servers[0] != "kagi" {
		t.Fatalf("Servers = %#v, want [kagi]", response.Servers)
	}
}

func TestToolInit_ReconcilesExistingOpenCodeConfig(t *testing.T) {
	registry := server.NewRegistry(nil, nil)
	if err := registry.Add("kagi", &config.ServerConfig{Port: 6279, Command: "uvx"}); err != nil {
		t.Fatalf("Add(kagi): %v", err)
	}
	registry.Get("kagi").State = server.StateRunning

	srv := &Server{registry: registry}
	path := filepath.Join(t.TempDir(), "opencode.jsonc")
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
	if len(response.ReconciledServers) != 1 || response.ReconciledServers[0] != "kagi" {
		t.Fatalf("ReconciledServers = %#v, want [kagi]", response.ReconciledServers)
	}
	if !response.BackedUp || response.BackupPath != path+".backup" {
		t.Fatalf("backup response = backed_up:%v path:%q, want true and %q", response.BackedUp, response.BackupPath, path+".backup")
	}
	backupData, err := os.ReadFile(response.BackupPath)
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", response.BackupPath, err)
	}
	if string(backupData) != string(data) {
		t.Fatalf("backup bytes changed: got %q, want %q", backupData, data)
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
	c.Add(&catalog.Entry{Name: "zebra", Description: "Last", CodemodeNamespace: "zebra-tools"})
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
	if got := response.Results[2].CodemodeNamespace; got != "zebra-tools" {
		t.Fatalf("zebra codemode_namespace = %q, want zebra-tools", got)
	}
	if got := response.Results[0].CodemodeNamespace; got != "alpha" {
		t.Fatalf("alpha codemode_namespace = %q, want alpha", got)
	}
}

func TestToolAddRemove_DoesNotSyncOpenCodeConfig(t *testing.T) {
	tmp := t.TempDir()
	serversPath := filepath.Join(tmp, "servers.yaml")
	explicitConfigPath := filepath.Join(tmp, "opencode.jsonc")
	sentinel := []byte("{\n  \"sentinel\": true\n}\n")
	if err := os.WriteFile(explicitConfigPath, sentinel, 0o644); err != nil {
		t.Fatalf("WriteFile(%q): %v", explicitConfigPath, err)
	}

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
	if registry.Get("kagi") == nil {
		t.Fatal("registry missing kagi after add")
	}
	if daemonCfg.Servers["kagi"] == nil {
		t.Fatal("daemon config missing kagi after add")
	}
	assertOpenCodeConfigUnchanged(t, explicitConfigPath, sentinel)
	assertNoUnintendedOpenCodeConfig(t, tmp, explicitConfigPath)

	removeResult, err := srv.toolRemove(context.Background(), mustJSON(t, map[string]any{"name": "kagi"}))
	if err != nil {
		t.Fatalf("toolRemove() error = %v", err)
	}
	var removeResp RemoveResponse
	decodeToolJSON(t, removeResult, &removeResp)
	if !removeResp.Success {
		t.Fatalf("toolRemove() success = false, error = %v", removeResp.Error)
	}
	if registry.Get("kagi") != nil {
		t.Fatal("registry still contains kagi after remove")
	}
	if _, ok := daemonCfg.Servers["kagi"]; ok {
		t.Fatal("daemon config still contains kagi after remove")
	}
	assertOpenCodeConfigUnchanged(t, explicitConfigPath, sentinel)
	assertNoUnintendedOpenCodeConfig(t, tmp, explicitConfigPath)
}

func assertOpenCodeConfigUnchanged(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", path, err)
	}
	if string(got) != string(want) {
		t.Fatalf("config %q changed: got %q, want %q", path, got, want)
	}
}

func assertNoUnintendedOpenCodeConfig(t *testing.T, dir, explicitPath string) {
	t.Helper()
	for _, name := range []string{".opencode.json", "opencode.json", "opencode.jsonc"} {
		path := filepath.Join(dir, name)
		if filepath.Clean(path) == filepath.Clean(explicitPath) {
			continue
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("unexpected guessed config %q: stat error = %v", path, err)
		}
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
