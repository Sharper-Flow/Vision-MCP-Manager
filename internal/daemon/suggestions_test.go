package daemon

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/catalog"
	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/config"
	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/server"
)

func TestCatalogSuggestionProviderSuggestsCapabilityOverlap(t *testing.T) {
	cat := catalog.New()
	cat.Add(&catalog.Entry{Name: "kagi", Capabilities: []string{"web-search", "search"}})
	cat.Add(&catalog.Entry{Name: "brave-search", Capabilities: []string{"web-search", "search"}, Source: "https://example.com/brave"})
	cat.Add(&catalog.Entry{Name: "alpha-search", Capabilities: []string{"web-search"}})
	cat.Add(&catalog.Entry{Name: "zeta-search", Capabilities: []string{"search"}})
	cat.Add(&catalog.Entry{Name: "context7", Capabilities: []string{"documentation"}})

	reg := server.NewRegistry(nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := reg.Add("brave-search", &config.ServerConfig{}); err != nil {
		t.Fatalf("Add brave-search: %v", err)
	}

	provider := newCatalogSuggestionProvider(cat, reg)
	got := provider.SuggestAlternatives(context.Background(), "kagi", "kagi_search_fetch")

	if len(got) != 3 {
		t.Fatalf("len(got) = %d, want 3: %+v", len(got), got)
	}
	if got[0].ServerName != "brave-search" {
		t.Fatalf("first suggestion = %q, want brave-search", got[0].ServerName)
	}
	if !got[0].Installed {
		t.Fatalf("brave-search Installed = false, want true")
	}
	if got[0].Source != "https://example.com/brave" {
		t.Fatalf("brave-search Source = %q", got[0].Source)
	}
	if got[0].Reason != "shares 2 capabilities: web-search, search" {
		t.Fatalf("brave-search Reason = %q", got[0].Reason)
	}

	// alpha-search and zeta-search have the same hit count, so name ascending wins.
	if got[1].ServerName != "alpha-search" || got[2].ServerName != "zeta-search" {
		t.Fatalf("tie order = [%s, %s], want [alpha-search, zeta-search]", got[1].ServerName, got[2].ServerName)
	}
	if got[1].Installed || got[2].Installed {
		t.Fatalf("expected non-installed tie suggestions: %+v", got[1:])
	}
}

func TestCatalogSuggestionProviderNoSuggestions(t *testing.T) {
	tests := []struct {
		name string
		cat  *catalog.Catalog
	}{
		{
			name: "missing failed server",
			cat: func() *catalog.Catalog {
				cat := catalog.New()
				cat.Add(&catalog.Entry{Name: "brave-search", Capabilities: []string{"web-search"}})
				return cat
			}(),
		},
		{
			name: "failed server has no capabilities",
			cat: func() *catalog.Catalog {
				cat := catalog.New()
				cat.Add(&catalog.Entry{Name: "kagi"})
				cat.Add(&catalog.Entry{Name: "brave-search", Capabilities: []string{"web-search"}})
				return cat
			}(),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider := newCatalogSuggestionProvider(tt.cat, nil)
			got := provider.SuggestAlternatives(context.Background(), "kagi", "kagi_search_fetch")
			if len(got) != 0 {
				t.Fatalf("got %d suggestions, want none: %+v", len(got), got)
			}
		})
	}
}
