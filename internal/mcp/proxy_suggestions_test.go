package mcp

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
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

func TestProxyToolHandlerConvertsAvailabilityErrorToToolResult(t *testing.T) {
	providerCalled := false
	provider := fallbackSuggestionProviderFunc(func(_ context.Context, failedServer, failedTool string) []FallbackSuggestion {
		providerCalled = true
		if failedServer != "kagi" {
			t.Fatalf("failedServer = %q, want kagi", failedServer)
		}
		if failedTool != "kagi_search_fetch" {
			t.Fatalf("failedTool = %q, want kagi_search_fetch", failedTool)
		}
		return []FallbackSuggestion{{ServerName: "brave-search", Capabilities: []string{"web-search"}, Installed: true}}
	})

	ps := &proxySession{
		serverName:         "kagi",
		logger:             slog.New(slog.NewTextHandler(io.Discard, nil)),
		shared:             true,
		requestTimeout:     time.Second,
		retryConfig:        RetryConfig{MaxAttempts: 1},
		circuitBreaker:     newCircuitBreaker(CircuitBreakerConfig{}, nil),
		suggestionProvider: provider,
	}

	handler := makeProxyToolHandler(ps, "kagi_search_fetch")
	result, err := handler(context.Background(), &sdkmcp.CallToolRequest{
		Params: &sdkmcp.CallToolParamsRaw{Name: "kagi_search_fetch"},
	})
	if err != nil {
		t.Fatalf("expected nil Go error, got %v", err)
	}
	if result == nil {
		t.Fatal("expected tool result, got nil")
	}
	if !result.IsError {
		t.Fatal("expected IsError=true")
	}
	if !providerCalled {
		t.Fatal("expected suggestion provider to be called")
	}

	text := firstTextContent(t, result)
	for _, want := range []string{"config_drift", "kagi_search_fetch", "brave-search", "Vision did not retry on a different server"} {
		if !strings.Contains(text, want) {
			t.Fatalf("result text missing %q:\n%s", want, text)
		}
	}
}

func TestProxySessionFinishToolCallPreservesNonAvailabilityErrors(t *testing.T) {
	ps := &proxySession{serverName: "kagi"}
	wantErr := errors.New("tool validation failed")

	result, err := ps.finishToolCall(context.Background(), "kagi_search_fetch", nil, wantErr)
	if result != nil {
		t.Fatalf("expected nil result, got %#v", result)
	}
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
}
