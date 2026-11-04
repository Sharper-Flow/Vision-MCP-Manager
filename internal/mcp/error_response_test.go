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
			result := availabilityErrorToResult(availabilityErr, "kagi", "kagi_search_fetch")

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
			) {
				if !strings.Contains(text, want) {
					t.Fatalf("result text missing %q:\n%s", want, text)
				}
			}
		})
	}
}

func TestFormatAvailabilityFailureWithUnknownCategory(t *testing.T) {
	text := formatAvailabilityFailure(FailureCategory("new_category"), "server", "tool", "")

	for _, want := range []string{"new_category", "Tool 'tool' on server 'server' failed"} {
		if !strings.Contains(text, want) {
			t.Fatalf("formatted text missing %q:\n%s", want, text)
		}
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
