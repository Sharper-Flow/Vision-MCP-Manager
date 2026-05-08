package mcp

import (
	"fmt"
	"strings"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

func availabilityErrorToResult(err *AvailabilityError, server, tool string, suggestions []FallbackSuggestion) *sdkmcp.CallToolResult {
	if err == nil {
		return nil
	}

	result := &sdkmcp.CallToolResult{}
	result.SetError(err)
	result.Content = []sdkmcp.Content{
		&sdkmcp.TextContent{Text: formatAvailabilityFailureWithCause(err.Category, server, tool, failureCause(err), suggestions)},
	}
	return result
}

func formatAvailabilityFailure(category FailureCategory, server, tool string, suggestions []FallbackSuggestion) string {
	return formatAvailabilityFailureWithCause(category, server, tool, "", suggestions)
}

func formatAvailabilityFailureWithCause(category FailureCategory, server, tool, cause string, suggestions []FallbackSuggestion) string {
	var b strings.Builder

	fmt.Fprintf(&b, "Tool '%s' on server '%s' failed: %s", tool, server, category)
	if cause != "" {
		fmt.Fprintf(&b, " (%s)", cause)
	}
	b.WriteString(".\n")
	b.WriteString(failureCategoryGuidance(category))
	b.WriteString("\n")
	b.WriteString("Vision did not retry on a different server — agent must explicitly choose.\n\n")

	if len(suggestions) == 0 {
		b.WriteString("No fallback suggestions available.")
		return b.String()
	}

	b.WriteString("Suggested alternatives (capability overlap):\n")
	for i, suggestion := range suggestions {
		fmt.Fprintf(&b, "  %d. %s", i+1, suggestion.ServerName)
		if len(suggestion.Capabilities) > 0 {
			fmt.Fprintf(&b, " — capabilities: %s", strings.Join(suggestion.Capabilities, ", "))
		}
		fmt.Fprintf(&b, "; installed: %t", suggestion.Installed)
		if suggestion.Source != "" {
			fmt.Fprintf(&b, "; source: %s", suggestion.Source)
		}
		if suggestion.Reason != "" {
			fmt.Fprintf(&b, "; reason: %s", suggestion.Reason)
		}
		if i < len(suggestions)-1 {
			b.WriteString("\n")
		}
	}

	return b.String()
}

func failureCategoryGuidance(category FailureCategory) string {
	switch category {
	case FailureCategoryProviderTimeout:
		return "The upstream provider timed out before returning a tool result."
	case FailureCategoryCircuitOpen:
		return "The downstream circuit breaker is open after repeated failures."
	case FailureCategoryRetryExhausted:
		return "Vision exhausted configured retries for this downstream tool call."
	case FailureCategoryConfigDrift:
		return "The downstream session became unavailable or no longer matches Vision's runtime state."
	default:
		return "Vision classified this as an upstream availability failure."
	}
}

func failureCause(err *AvailabilityError) string {
	if err == nil || err.Err == nil {
		return ""
	}
	return err.Err.Error()
}
