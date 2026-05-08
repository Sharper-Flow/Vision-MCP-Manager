package mcp

import (
	"context"
	"errors"
	"strings"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestAvailabilityErrorToResult(t *testing.T) {
	tests := []struct {
		name     string
		category FailureCategory
		cause    error
		wantText []string
	}{
		{
			name:     "provider timeout",
			category: FailureCategoryProviderTimeout,
			cause:    context.DeadlineExceeded,
			wantText: []string{"provider_timeout", "deadline exceeded"},
		},
		{
			name:     "circuit open",
			category: FailureCategoryCircuitOpen,
			cause:    &CircuitOpenError{Server: "kagi"},
			wantText: []string{"circuit_open", "downstream circuit breaker open"},
		},
		{
			name:     "retry exhausted",
			category: FailureCategoryRetryExhausted,
			cause:    errors.New("upstream returned 503"),
			wantText: []string{"retry_exhausted", "upstream returned 503"},
		},
		{
			name:     "config drift",
			category: FailureCategoryConfigDrift,
			cause:    ErrDownstreamUnavailable,
			wantText: []string{"config_drift", "downstream session unavailable"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			availabilityErr := &AvailabilityError{Category: tt.category, Err: tt.cause}
			result := availabilityErrorToResult(availabilityErr, "kagi", "kagi_search_fetch", nil)

			if result == nil {
				t.Fatal("expected result, got nil")
			}
			if !result.IsError {
				t.Fatal("expected IsError=true")
			}
			if result.GetError() == nil {
				t.Fatal("expected SetError to preserve internal error")
			}

			text := firstTextContent(t, result)
			for _, want := range append(tt.wantText,
				"Tool 'kagi_search_fetch' on server 'kagi' failed",
				"Vision did not retry on a different server",
				"agent must explicitly choose",
				"No fallback suggestions available",
			) {
				if !strings.Contains(text, want) {
					t.Fatalf("result text missing %q:\n%s", want, text)
				}
			}
		})
	}
}

func TestAvailabilityErrorToResultWithSuggestions(t *testing.T) {
	availabilityErr := &AvailabilityError{Category: FailureCategoryProviderTimeout, Err: context.DeadlineExceeded}
	suggestions := []FallbackSuggestion{
		{
			ServerName:   "brave-search",
			Capabilities: []string{"web-search", "search"},
			Installed:    true,
			Source:       "https://example.com/brave-search",
			Reason:       "shares 2 capabilities: web-search, search",
		},
		{
			ServerName:   "tavily",
			Capabilities: []string{"web-search"},
			Installed:    false,
			Reason:       "shares 1 capability: web-search",
		},
	}

	result := availabilityErrorToResult(availabilityErr, "kagi", "kagi_search_fetch", suggestions)
	text := firstTextContent(t, result)

	for _, want := range []string{
		"Suggested alternatives (capability overlap)",
		"1. brave-search",
		"capabilities: web-search, search",
		"installed: true",
		"source: https://example.com/brave-search",
		"shares 2 capabilities: web-search, search",
		"2. tavily",
		"installed: false",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("result text missing %q:\n%s", want, text)
		}
	}
}

func TestFormatAvailabilityFailureWithUnknownCategory(t *testing.T) {
	text := formatAvailabilityFailure(FailureCategory("new_category"), "server", "tool", nil)

	for _, want := range []string{"new_category", "Tool 'tool' on server 'server' failed"} {
		if !strings.Contains(text, want) {
			t.Fatalf("formatted text missing %q:\n%s", want, text)
		}
	}
}

func TestSanitizeSource(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"https://example.com/brave", "https://example.com/brave"},
		{"https://example.com/a\x00b", "https://example.com/ab"},
		{"https://example.com/\x1b[31mred", "https://example.com/[31mred"},
		{"a\x7fb", "ab"},
		{"normal-url", "normal-url"},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := sanitizeSource(tt.input)
			if got != tt.want {
				t.Fatalf("sanitizeSource(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestAvailabilityErrorToResultSanitizesSourceURL(t *testing.T) {
	availabilityErr := &AvailabilityError{Category: FailureCategoryProviderTimeout, Err: context.DeadlineExceeded}
	suggestions := []FallbackSuggestion{
		{ServerName: "evil", Source: "https://example.com/\x00injected"},
	}

	result := availabilityErrorToResult(availabilityErr, "kagi", "tool", suggestions)
	text := firstTextContent(t, result)

	if strings.Contains(text, "\x00") {
		t.Fatal("control character leaked into output")
	}
	if !strings.Contains(text, "https://example.com/injected") {
		t.Fatalf("sanitized source missing from output:\n%s", text)
	}
}

func firstTextContent(t *testing.T, result *sdkmcp.CallToolResult) string {
	t.Helper()
	if result == nil {
		t.Fatal("expected result, got nil")
	}
	if len(result.Content) == 0 {
		t.Fatal("expected content")
	}
	text, ok := result.Content[0].(*sdkmcp.TextContent)
	if !ok {
		t.Fatalf("expected TextContent, got %T", result.Content[0])
	}
	return text.Text
}
