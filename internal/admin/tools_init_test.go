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
	"github.com/tailscale/hujson"
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

func TestToolInit_RejectsRelativeExplicitPathBeforeRegistryOrFilesystem(t *testing.T) {
	workingDir := t.TempDir()
	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd(): %v", err)
	}
	if err := os.Chdir(workingDir); err != nil {
		t.Fatalf("Chdir(%q): %v", workingDir, err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldDir) })

	srv := &Server{}
	_, err = srv.toolInit(context.Background(), mustJSON(t, map[string]any{"path": "relative.jsonc"}))
	var validationErr *ValidationError
	if !errors.As(err, &validationErr) {
		t.Fatalf("toolInit() error = %v, want validation error before registry access", err)
	}
	if _, statErr := os.Stat(filepath.Join(workingDir, "relative.jsonc")); !os.IsNotExist(statErr) {
		t.Fatalf("relative path was touched: stat error = %v", statErr)
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

func TestToolInit_ReconcilesLosslessJSONCComments(t *testing.T) {
	registry := server.NewRegistry(nil, nil)
	if err := registry.Add("kagi", &config.ServerConfig{Port: 6279, Command: "uvx"}); err != nil {
		t.Fatalf("Add(kagi): %v", err)
	}
	registry.Get("kagi").State = server.StateRunning

	srv := &Server{registry: registry}
	path := filepath.Join(t.TempDir(), "opencode.jsonc")
	original := []byte(`{
  // before member: keep-before
  "theme" /* between name and colon: keep-name */ : /* colon and value: keep-colon */ "ayu-dark" /* value and comma: keep-value */, // trailing line: keep-line
  /* unrelated member comment: keep-unrelated */
  "mcp": {
    /* before server: keep-server */
    "kagi": {"type": "remote", "url": "http://localhost:6283/mcp", "enabled": true},
    "custom": {"type": "remote", "url": "http://localhost:9999/mcp", "enabled": true} /* trailing block: keep-block */,
  },
}
`)
	if err := os.WriteFile(path, original, 0o644); err != nil {
		t.Fatalf("WriteFile(%q): %v", path, err)
	}

	result, err := srv.toolInit(context.Background(), mustJSON(t, map[string]any{"path": path, "servers": "kagi"}))
	if err != nil {
		t.Fatalf("toolInit() error = %v", err)
	}
	var response InitResponse
	decodeToolJSON(t, result, &response)
	if !response.Success {
		t.Fatalf("toolInit() success = false, error = %v", response.Error)
	}

	backup, err := os.ReadFile(path + ".backup")
	if err != nil {
		t.Fatalf("ReadFile backup: %v", err)
	}
	if string(backup) != string(original) {
		t.Fatalf("backup bytes changed: got %q, want %q", backup, original)
	}
	updated, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile updated config: %v", err)
	}
	for _, marker := range []string{
		"keep-before", "keep-name", "keep-colon", "keep-value", "keep-line",
		"keep-unrelated", "keep-server", "keep-block",
		`"url": "http://localhost:6279/mcp"`,
		`"url": "http://localhost:9999/mcp"`,
	} {
		if !strings.Contains(string(updated), marker) {
			t.Errorf("updated config lost marker %q: %s", marker, updated)
		}
	}
	oldValue := `{"type": "remote", "url": "http://localhost:6283/mcp", "enabled": true}`
	oldValueStart := strings.Index(string(original), oldValue)
	if oldValueStart < 0 {
		t.Fatal("test fixture missing selected value")
	}
	oldValueEnd := oldValueStart + len(oldValue)
	if !strings.Contains(string(updated), string(original[:oldValueStart])) {
		t.Fatal("replacement changed prefix before selected value")
	}
	if !strings.Contains(string(updated), string(original[oldValueEnd:])) {
		t.Fatal("replacement changed suffix after selected value")
	}
}

func TestToolInit_InsertsMCPAndPreservesRootTrailingComma(t *testing.T) {
	srv := &Server{registry: runningRegistry(t, "a/b", "a~b")}
	path := filepath.Join(t.TempDir(), "opencode.jsonc")
	original := []byte(`{
  // keep root comment
	  "theme": "ayu-dark",
  /* root closing marker: keep-root-close */
}
`)
	if err := os.WriteFile(path, original, 0o644); err != nil {
		t.Fatalf("WriteFile(%q): %v", path, err)
	}

	result, err := srv.toolInit(context.Background(), mustJSON(t, map[string]any{"path": path}))
	if err != nil {
		t.Fatalf("toolInit() error = %v", err)
	}
	var response InitResponse
	decodeToolJSON(t, result, &response)
	if !response.Success {
		t.Fatalf("toolInit() success = false, error = %v", response.Error)
	}
	updated, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", path, err)
	}
	if !strings.Contains(string(updated), "keep root comment") {
		t.Fatalf("updated config lost root comment: %s", updated)
	}
	if !strings.Contains(string(updated), "keep-root-close") {
		t.Fatalf("updated config lost root closing marker: %s", updated)
	}
	value, err := hujson.Parse(updated)
	if err != nil {
		t.Fatalf("Parse updated config: %v", err)
	}
	root := value.Value.(*hujson.Object)
	if root.Members[len(root.Members)-1].Value.AfterExtra == nil {
		t.Fatalf("root trailing comma was not preserved: after=%q value-after=%#v: %s", root.AfterExtra, root.Members[len(root.Members)-1].Value.AfterExtra, updated)
	}
	assertGeneratedEntry(t, updated, "a/b")
	assertGeneratedEntry(t, updated, "a~b")
}

func TestToolInit_InsertsMCPIntoEmptyRootWithoutDuplicatingMarker(t *testing.T) {
	srv := &Server{registry: runningRegistry(t, "a/b", "a~b")}
	path := filepath.Join(t.TempDir(), "opencode.jsonc")
	if err := os.WriteFile(path, []byte(`{ /* root-empty-marker */ }`), 0o644); err != nil {
		t.Fatalf("WriteFile(%q): %v", path, err)
	}
	result, err := srv.toolInit(context.Background(), mustJSON(t, map[string]any{"path": path}))
	if err != nil {
		t.Fatalf("toolInit() error = %v", err)
	}
	var response InitResponse
	decodeToolJSON(t, result, &response)
	if !response.Success {
		t.Fatalf("toolInit() success = false, error = %v", response.Error)
	}
	updated, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", path, err)
	}
	if got := strings.Count(string(updated), "root-empty-marker"); got != 1 {
		t.Fatalf("root marker count = %d, want 1: %s", got, updated)
	}
	if _, err := hujson.Parse(updated); err != nil {
		t.Fatalf("Parse updated config: %v", err)
	}
	assertGeneratedEntry(t, updated, "a/b")
	assertGeneratedEntry(t, updated, "a~b")
}

func TestToolInit_InsertsServersIntoEmptyMCPWithoutDuplicatingMarker(t *testing.T) {
	srv := &Server{registry: runningRegistry(t, "a/b", "a~b")}
	path := filepath.Join(t.TempDir(), "opencode.jsonc")
	if err := os.WriteFile(path, []byte(`{"mcp": { /* mcp-empty-marker */ }}`), 0o644); err != nil {
		t.Fatalf("WriteFile(%q): %v", path, err)
	}
	result, err := srv.toolInit(context.Background(), mustJSON(t, map[string]any{"path": path}))
	if err != nil {
		t.Fatalf("toolInit() error = %v", err)
	}
	var response InitResponse
	decodeToolJSON(t, result, &response)
	if !response.Success {
		t.Fatalf("toolInit() success = false, error = %v", response.Error)
	}
	updated, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", path, err)
	}
	if got := strings.Count(string(updated), "mcp-empty-marker"); got != 1 {
		t.Fatalf("mcp marker count = %d, want 1: %s", got, updated)
	}
	if _, err := hujson.Parse(updated); err != nil {
		t.Fatalf("Parse updated config: %v", err)
	}
	assertGeneratedEntry(t, updated, "a/b")
	assertGeneratedEntry(t, updated, "a~b")
}

func TestToolInit_InsertsMissingServerAndPreservesMCPTrailingComma(t *testing.T) {
	srv := &Server{registry: runningRegistry(t, "a/b", "a~b")}
	path := filepath.Join(t.TempDir(), "opencode.jsonc")
	original := []byte(`{
  "mcp": {
    // keep mcp comment
    "custom": {"type": "remote", "url": "http://localhost:9999/mcp", "enabled": true},
    /* mcp closing marker: keep-mcp-close */
  },
}
`)
	if err := os.WriteFile(path, original, 0o644); err != nil {
		t.Fatalf("WriteFile(%q): %v", path, err)
	}

	result, err := srv.toolInit(context.Background(), mustJSON(t, map[string]any{"path": path}))
	if err != nil {
		t.Fatalf("toolInit() error = %v", err)
	}
	var response InitResponse
	decodeToolJSON(t, result, &response)
	if !response.Success {
		t.Fatalf("toolInit() success = false, error = %v", response.Error)
	}
	updated, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", path, err)
	}
	if !strings.Contains(string(updated), "keep mcp comment") || !strings.Contains(string(updated), "localhost:9999") {
		t.Fatalf("updated config lost existing markers: %s", updated)
	}
	if !strings.Contains(string(updated), "keep-mcp-close") {
		t.Fatalf("updated config lost mcp closing marker: %s", updated)
	}
	value, err := hujson.Parse(updated)
	if err != nil {
		t.Fatalf("Parse updated config: %v", err)
	}
	root := value.Value.(*hujson.Object)
	mcp := findObjectMember(root, "mcp")
	mcpObject := mcp.Value.Value.(*hujson.Object)
	if mcpObject.Members[len(mcpObject.Members)-1].Value.AfterExtra == nil {
		t.Fatalf("mcp trailing comma was not preserved: after=%q value-after=%#v: %s", mcpObject.AfterExtra, mcpObject.Members[len(mcpObject.Members)-1].Value.AfterExtra, updated)
	}
	assertGeneratedEntry(t, updated, "a/b")
	assertGeneratedEntry(t, updated, "a~b")
}

func TestToolInit_InsertsMissingServerWithoutTrailingComma(t *testing.T) {
	srv := &Server{registry: runningRegistry(t, "a/b", "a~b")}
	path := filepath.Join(t.TempDir(), "opencode.jsonc")
	original := []byte(`{
  "mcp": {
    "custom": {"type": "remote", "url": "http://localhost:9999/mcp", "enabled": true}
  }
}
`)
	if err := os.WriteFile(path, original, 0o644); err != nil {
		t.Fatalf("WriteFile(%q): %v", path, err)
	}

	result, err := srv.toolInit(context.Background(), mustJSON(t, map[string]any{"path": path}))
	if err != nil {
		t.Fatalf("toolInit() error = %v", err)
	}
	var response InitResponse
	decodeToolJSON(t, result, &response)
	if !response.Success {
		t.Fatalf("toolInit() success = false, error = %v", response.Error)
	}
	updated, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", path, err)
	}
	value, err := hujson.Parse(updated)
	if err != nil {
		t.Fatalf("Parse updated config: %v", err)
	}
	root := value.Value.(*hujson.Object)
	mcp := findObjectMember(root, "mcp")
	mcpObject := mcp.Value.Value.(*hujson.Object)
	if mcpObject.Members[len(mcpObject.Members)-1].Value.AfterExtra != nil {
		t.Fatalf("unexpected mcp trailing comma: %s", updated)
	}
	if strings.Contains(string(updated), `"enabled": true},\n  }`) {
		t.Fatalf("textual output has trailing comma after inserted server: %s", updated)
	}
	assertGeneratedEntry(t, updated, "a/b")
	assertGeneratedEntry(t, updated, "a~b")
}

func assertGeneratedEntry(t *testing.T, data []byte, name string) {
	t.Helper()
	standard, err := hujson.Standardize(data)
	if err != nil {
		t.Fatalf("Standardize generated config: %v", err)
	}
	var config map[string]any
	if err := json.Unmarshal(standard, &config); err != nil {
		t.Fatalf("Unmarshal generated config: %v", err)
	}
	mcp, ok := config["mcp"].(map[string]any)
	if !ok {
		t.Fatalf("generated config missing mcp object: %#v", config)
	}
	entry, ok := mcp[name].(map[string]any)
	if !ok {
		t.Fatalf("generated config missing %q entry: %#v", name, mcp)
	}
	if entry["type"] != "remote" || entry["url"] != "http://localhost:6279/mcp" || entry["enabled"] != true {
		t.Fatalf("generated %q entry = %#v", name, entry)
	}
}

func TestToolInit_InvalidJSONCLeavesBytesUnchanged(t *testing.T) {
	registry := runningRegistry(t, "kagi")
	srv := &Server{registry: registry}
	path := filepath.Join(t.TempDir(), "opencode.jsonc")
	original := []byte("{ invalid jsonc\n")
	if err := os.WriteFile(path, original, 0o644); err != nil {
		t.Fatalf("WriteFile(%q): %v", path, err)
	}

	result, err := srv.toolInit(context.Background(), mustJSON(t, map[string]any{"path": path}))
	if err != nil {
		t.Fatalf("toolInit() error = %v, want structured failure", err)
	}
	var response InitResponse
	decodeToolJSON(t, result, &response)
	if response.Success {
		t.Fatal("toolInit() success = true for invalid JSONC")
	}
	assertOpenCodeConfigUnchanged(t, path, original)
}

func TestToolInit_NonObjectMCPLeavesBytesUnchanged(t *testing.T) {
	registry := runningRegistry(t, "kagi")
	srv := &Server{registry: registry}
	path := filepath.Join(t.TempDir(), "opencode.json")
	original := []byte(`{"theme":"ayu-dark","mcp":[]}`)
	if err := os.WriteFile(path, original, 0o644); err != nil {
		t.Fatalf("WriteFile(%q): %v", path, err)
	}

	result, err := srv.toolInit(context.Background(), mustJSON(t, map[string]any{"path": path}))
	if err != nil {
		t.Fatalf("toolInit() error = %v, want structured failure", err)
	}
	var response InitResponse
	decodeToolJSON(t, result, &response)
	if response.Success {
		t.Fatal("toolInit() success = true for non-object mcp")
	}
	assertOpenCodeConfigUnchanged(t, path, original)
}

func TestToolInit_NonObjectRootLeavesBytesUnchanged(t *testing.T) {
	srv := &Server{registry: runningRegistry(t, "kagi")}
	dir := t.TempDir()
	path := filepath.Join(dir, "opencode.jsonc")
	original := []byte(`["not", "an", "object"]`)
	if err := os.WriteFile(path, original, 0o644); err != nil {
		t.Fatalf("WriteFile(%q): %v", path, err)
	}

	result, err := srv.toolInit(context.Background(), mustJSON(t, map[string]any{"path": path}))
	if err != nil {
		t.Fatalf("toolInit() error = %v, want structured failure", err)
	}
	var response InitResponse
	decodeToolJSON(t, result, &response)
	if response.Success {
		t.Fatal("toolInit() success = true for non-object root")
	}
	assertOpenCodeConfigUnchanged(t, path, original)
	if _, err := os.Stat(path + ".backup"); !os.IsNotExist(err) {
		t.Fatalf("unexpected backup %q: stat error = %v", path+".backup", err)
	}
}

func TestToolInit_QuotesSlashAndTildeServerNames(t *testing.T) {
	registry := server.NewRegistry(nil, nil)
	for _, name := range []string{"a/b", "a~b"} {
		if err := registry.Add(name, &config.ServerConfig{Port: 6279, Command: "uvx"}); err != nil {
			t.Fatalf("Add(%q): %v", name, err)
		}
		registry.Get(name).State = server.StateRunning
	}

	path := filepath.Join(t.TempDir(), "opencode.json")
	result, err := (&Server{registry: registry}).toolInit(context.Background(), mustJSON(t, map[string]any{"path": path}))
	if err != nil {
		t.Fatalf("toolInit() error = %v", err)
	}
	var response InitResponse
	decodeToolJSON(t, result, &response)
	if !response.Success {
		t.Fatalf("toolInit() success = false, error = %v", response.Error)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", path, err)
	}
	for _, name := range []string{`"a/b"`, `"a~b"`} {
		if !strings.Contains(string(data), name) {
			t.Errorf("generated config missing literal key %s: %s", name, data)
		}
	}
}

func runningRegistry(t *testing.T, names ...string) *server.Registry {
	t.Helper()
	registry := server.NewRegistry(nil, nil)
	for _, name := range names {
		if err := registry.Add(name, &config.ServerConfig{Port: 6279, Command: "uvx"}); err != nil {
			t.Fatalf("Add(%q): %v", name, err)
		}
		registry.Get(name).State = server.StateRunning
	}
	return registry
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
