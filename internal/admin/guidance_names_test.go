package admin

import (
	"testing"

	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/config"
)

func TestDeriveNamespacedName(t *testing.T) {
	tests := []struct {
		name    string
		tool    string
		servers []string
		want    string
	}{
		{name: "dot safe", tool: "lgrep_search_semantic", servers: []string{"lgrep"}, want: "tools.lgrep.search_semantic"},
		{name: "hyphen uses brackets", tool: "context7_resolve-library-id", servers: []string{"context7"}, want: `tools.context7["resolve-library-id"]`},
		{name: "leading digit uses brackets", tool: "time_24hour", servers: []string{"time"}, want: `tools.time["24hour"]`},
		{name: "namespace uses brackets", tool: "my-server_lookup", servers: []string{"my-server"}, want: `tools["my-server"].lookup`},
		{name: "longest prefix wins", tool: "search_code_lookup", servers: []string{"search", "search_code"}, want: "tools.search_code.lookup"},
		{name: "no matching server", tool: "unknown_lookup", servers: []string{"search"}, want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := deriveNamespacedName(tt.tool, tt.servers); got != tt.want {
				t.Fatalf("deriveNamespacedName() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestToolInstructionsToEntry_NamespacedNamePrecedence(t *testing.T) {
	s := &Server{instructions: &config.Instructions{
		Servers: map[string]*config.ServerInstructions{"lgrep": {}},
	}}

	derived := s.toolInstructionsToEntry("lgrep_search_semantic", &config.ToolInstructions{})
	if derived.NamespacedName != "tools.lgrep.search_semantic" {
		t.Fatalf("derived namespaced name = %q", derived.NamespacedName)
	}

	explicit := s.toolInstructionsToEntry("lgrep_search_semantic", &config.ToolInstructions{
		NamespacedName: "tools.custom.lookup",
	})
	if explicit.NamespacedName != "tools.custom.lookup" {
		t.Fatalf("explicit namespaced name = %q", explicit.NamespacedName)
	}
}
