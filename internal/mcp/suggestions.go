package mcp

import "context"

// FallbackSuggestionProvider supplies alternative MCP servers for a failed
// tool call. Implementations should be side-effect free; callers decide
// whether to use any returned suggestion.
type FallbackSuggestionProvider interface {
	SuggestAlternatives(ctx context.Context, failedServer, failedTool string) []FallbackSuggestion
}

// FallbackSuggestion describes one alternative server that may satisfy the
// same capability as a failed tool call. It is advisory only; Vision never
// invokes suggested alternatives automatically.
type FallbackSuggestion struct {
	ServerName   string
	Capabilities []string
	Installed    bool
	Source       string
	Reason       string
}
