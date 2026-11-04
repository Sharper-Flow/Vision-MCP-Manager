package admin

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/config"
	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/server"
)

const removeFixtureBefore = `# Operator registry
servers:
  # search
  kagi:
    command: uvx
    args: ["kagimcp"]
    env:
      KAGI_API_KEY: ${VSN46_KAGI_KEY}
    port: 6279

  # docs, keep this comment
  context7:
    command: npx
    args:
      - -y
      - "@upstash/context7-mcp"
      - --api-key
      - ${VSN46_CONTEXT7_KEY}
    port: 6280
`

// toolRemove must edit only the named server in servers.yaml: every other
// server, ${VAR} reference and comment stays byte-identical, including
// servers added to the file after the daemon loaded its config.
func TestToolRemove_PreservesOtherServersReferencesAndComments(t *testing.T) {
	t.Setenv("VSN46_KAGI_KEY", "kagi-secret-value")
	t.Setenv("VSN46_CONTEXT7_KEY", "context7-secret-value")

	path := filepath.Join(t.TempDir(), "servers.yaml")
	if err := os.WriteFile(path, []byte(removeFixtureBefore), 0o640); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}

	// The operator edits the file after the daemon loaded it, as a reload
	// would pick up. The admin server still holds the startup config.
	edited := removeFixtureBefore + `
  # added after startup
  time:
    command: npx
    args: ["-y", "@anthropic/mcp-time"]
    port: 6281
`
	if err := os.WriteFile(path, []byte(edited), 0o640); err != nil {
		t.Fatal(err)
	}

	registry := server.NewRegistry(nil, nil)
	if err := registry.Add("kagi", loaded.Servers["kagi"]); err != nil {
		t.Fatal(err)
	}
	srv := &Server{registry: registry, daemonConfig: loaded, configPath: path}

	result, err := srv.toolRemove(context.Background(), mustJSON(t, map[string]any{"name": "kagi"}))
	if err != nil {
		t.Fatal(err)
	}
	var resp RemoveResponse
	decodeToolJSON(t, result, &resp)
	if !resp.Success {
		t.Fatalf("toolRemove() success = false, error = %v", resp.Error)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := `# Operator registry
servers:
  # docs, keep this comment
  context7:
    command: npx
    args:
      - -y
      - "@upstash/context7-mcp"
      - --api-key
      - ${VSN46_CONTEXT7_KEY}
    port: 6280

  # added after startup
  time:
    command: npx
    args: ["-y", "@anthropic/mcp-time"]
    port: 6281
`
	if string(got) != want {
		t.Fatalf("servers.yaml after remove:\n%s\nwant:\n%s", got, want)
	}
	if strings.Contains(string(got), "secret-value") {
		t.Fatalf("servers.yaml contains a resolved secret:\n%s", got)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("servers.yaml mode = %v, want 0640", info.Mode().Perm())
	}
}
