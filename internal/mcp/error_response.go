package mcp

import (
	"fmt"
	"strings"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

func availabilityErrorToResult(err *AvailabilityError, server, tool string) *sdkmcp.CallToolResult {
	if err == nil {
		return nil
	}

	result := &sdkmcp.CallToolResult{}
	result.SetError(err)
	result.Content = []sdkmcp.Content{
		&sdkmcp.TextContent{Text: formatAvailabilityFailure(err.Category, server, tool, failureCause(err))},
	}
	return result
}

func formatAvailabilityFailure(category FailureCategory, server, tool, cause string) string {
	var b strings.Builder

	fmt.Fprintf(&b, "Tool '%s' on server '%s' failed: %s", tool, server, category)
	if cause != "" {
		fmt.Fprintf(&b, " (%s)", cause)
	}
	b.WriteString(".\n")
	b.WriteString(failureCategoryGuidance(category))
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
