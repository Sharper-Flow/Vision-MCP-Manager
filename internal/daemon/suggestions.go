package daemon

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/catalog"
	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/mcp"
	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/server"
)

type catalogSuggestionProvider struct {
	catalog  *catalog.Catalog
	registry *server.Registry
}

func newCatalogSuggestionProvider(cat *catalog.Catalog, registry *server.Registry) *catalogSuggestionProvider {
	return &catalogSuggestionProvider{catalog: cat, registry: registry}
}

func (p *catalogSuggestionProvider) SuggestAlternatives(_ context.Context, failedServer, _ string) []mcp.FallbackSuggestion {
	if p == nil || p.catalog == nil {
		return nil
	}

	failed := p.catalog.Get(failedServer)
	if failed == nil || len(failed.Capabilities) == 0 {
		return nil
	}

	hits := make(map[string]int)
	shared := make(map[string][]string)
	entries := make(map[string]*catalog.Entry)
	seenCapabilities := make(map[string]struct{})

	for _, capability := range failed.Capabilities {
		capability = strings.TrimSpace(capability)
		if capability == "" {
			continue
		}
		capabilityKey := strings.ToLower(capability)
		if _, ok := seenCapabilities[capabilityKey]; ok {
			continue
		}
		seenCapabilities[capabilityKey] = struct{}{}

		for _, entry := range p.catalog.Search("", capability) {
			if entry == nil || entry.Name == "" || entry.Name == failedServer {
				continue
			}
			if !entry.HasCapability(capability) {
				continue
			}
			hits[entry.Name]++
			shared[entry.Name] = append(shared[entry.Name], capability)
			entries[entry.Name] = entry
		}
	}

	if len(hits) == 0 {
		return nil
	}

	names := make([]string, 0, len(hits))
	for name := range hits {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		if hits[names[i]] != hits[names[j]] {
			return hits[names[i]] > hits[names[j]]
		}
		return names[i] < names[j]
	})

	limit := len(names)
	if limit > 3 {
		limit = 3
	}

	suggestions := make([]mcp.FallbackSuggestion, 0, limit)
	for _, name := range names[:limit] {
		entry := entries[name]
		capabilities := shared[name]
		suggestions = append(suggestions, mcp.FallbackSuggestion{
			ServerName:   name,
			Capabilities: append([]string(nil), capabilities...),
			Installed:    p.registry != nil && p.registry.Get(name) != nil,
			Source:       entry.Source,
			Reason:       sharedCapabilitiesReason(capabilities),
		})
	}

	return suggestions
}

func sharedCapabilitiesReason(capabilities []string) string {
	if len(capabilities) == 1 {
		return fmt.Sprintf("shares 1 capability: %s", capabilities[0])
	}
	return fmt.Sprintf("shares %d capabilities: %s", len(capabilities), strings.Join(capabilities, ", "))
}
