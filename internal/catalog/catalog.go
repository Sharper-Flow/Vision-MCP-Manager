// Package catalog provides a searchable registry of available MCP servers.
// Unlike the runtime registry (server.Registry), the catalog contains
// server definitions that can be discovered and installed.
package catalog

import (
	"sort"
	"strings"
)

// Entry represents an MCP server available for installation.
type Entry struct {
	// Name is the unique identifier for this server (e.g., "context7")
	Name string `json:"name" yaml:"name"`

	// Description is a human-readable description of the server's purpose
	Description string `json:"description" yaml:"description"`

	// Capabilities are tags describing what this server can do
	// (e.g., ["documentation", "code-search", "library-lookup"])
	Capabilities []string `json:"capabilities" yaml:"capabilities"`

	// Command is the executable to run (for stdio transport)
	Command string `json:"command,omitempty" yaml:"command,omitempty"`

	// Args are command-line arguments
	Args []string `json:"args,omitempty" yaml:"args,omitempty"`

	// EnvVars are required environment variables (names only, not values)
	EnvVars []string `json:"env_vars,omitempty" yaml:"env_vars,omitempty"`

	// URL is the upstream URL (for http/sse transport)
	URL string `json:"url,omitempty" yaml:"url,omitempty"`

	// Source is where to find more information (e.g., GitHub URL)
	Source string `json:"source,omitempty" yaml:"source,omitempty"`

	// Transport is "stdio", "http", or "sse" (default: stdio)
	Transport string `json:"transport,omitempty" yaml:"transport,omitempty"`
}

// Catalog is a searchable collection of server entries.
type Catalog struct {
	entries map[string]*Entry
}

// New creates a new empty catalog.
func New() *Catalog {
	return &Catalog{
		entries: make(map[string]*Entry),
	}
}

// Add registers a server entry in the catalog.
func (c *Catalog) Add(entry *Entry) {
	if entry == nil || entry.Name == "" {
		return
	}
	c.entries[entry.Name] = entry
}

// Get retrieves a server entry by name.
func (c *Catalog) Get(name string) *Entry {
	return c.entries[name]
}

// List returns all entries sorted by name.
func (c *Catalog) List() []*Entry {
	entries := make([]*Entry, 0, len(c.entries))
	for _, e := range c.entries {
		entries = append(entries, e)
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name < entries[j].Name
	})
	return entries
}

// Search finds entries matching the query and/or capability filter.
// If query is empty, all entries are considered.
// If capability is provided, only entries with that capability are returned.
func (c *Catalog) Search(query, capability string) []*Entry {
	query = strings.ToLower(strings.TrimSpace(query))
	capability = strings.ToLower(strings.TrimSpace(capability))

	var results []*Entry
	for _, entry := range c.entries {
		// Check capability filter first
		if capability != "" && !entry.HasCapability(capability) {
			continue
		}

		// If no query, include all (that passed capability filter)
		if query == "" {
			results = append(results, entry)
			continue
		}

		// Match against name, description, or capabilities
		if entry.Matches(query) {
			results = append(results, entry)
		}
	}

	// Sort by relevance (exact name match first, then by name)
	sort.Slice(results, func(i, j int) bool {
		// Exact name match gets priority
		iExact := strings.ToLower(results[i].Name) == query
		jExact := strings.ToLower(results[j].Name) == query
		if iExact && !jExact {
			return true
		}
		if jExact && !iExact {
			return false
		}
		// Otherwise sort by name
		return results[i].Name < results[j].Name
	})

	return results
}

// Count returns the number of entries in the catalog.
func (c *Catalog) Count() int {
	return len(c.entries)
}

// HasCapability checks if the entry has the given capability (case-insensitive).
func (e *Entry) HasCapability(cap string) bool {
	cap = strings.ToLower(cap)
	for _, c := range e.Capabilities {
		if strings.ToLower(c) == cap {
			return true
		}
	}
	return false
}

// Matches checks if the entry matches a search query (case-insensitive).
// Matches against name, description, and capabilities.
func (e *Entry) Matches(query string) bool {
	query = strings.ToLower(query)

	// Check name
	if strings.Contains(strings.ToLower(e.Name), query) {
		return true
	}

	// Check description
	if strings.Contains(strings.ToLower(e.Description), query) {
		return true
	}

	// Check capabilities
	for _, cap := range e.Capabilities {
		if strings.Contains(strings.ToLower(cap), query) {
			return true
		}
	}

	return false
}

// GetTransport returns the transport type, defaulting to "stdio".
func (e *Entry) GetTransport() string {
	if e.Transport == "" {
		return "stdio"
	}
	return e.Transport
}
