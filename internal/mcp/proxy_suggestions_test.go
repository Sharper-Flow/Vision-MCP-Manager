package mcp

import (
	"context"
	"testing"
)

type fallbackSuggestionProviderFunc func(context.Context, string, string) []FallbackSuggestion

func (f fallbackSuggestionProviderFunc) SuggestAlternatives(ctx context.Context, failedServer, failedTool string) []FallbackSuggestion {
	return f(ctx, failedServer, failedTool)
}

func TestProxyConfigSuggestionProviderPlumbingTypes(t *testing.T) {
	provider := fallbackSuggestionProviderFunc(func(_ context.Context, failedServer, failedTool string) []FallbackSuggestion {
		return []FallbackSuggestion{{ServerName: failedServer + "-alt", Reason: failedTool}}
	})

	cfg := ProxyConfig{
		ServerName:         "kagi",
		SuggestionProvider: provider,
	}
	ps := &proxySession{
		serverName:         cfg.ServerName,
		suggestionProvider: cfg.SuggestionProvider,
	}

	got := ps.suggestionProvider.SuggestAlternatives(context.Background(), ps.serverName, "kagi_search_fetch")
	if len(got) != 1 {
		t.Fatalf("expected one suggestion, got %d", len(got))
	}
	if got[0].ServerName != "kagi-alt" || got[0].Reason != "kagi_search_fetch" {
		t.Fatalf("unexpected suggestion: %+v", got[0])
	}
}
