// Package catalog provides a searchable registry of available MCP servers.
// Unlike the runtime registry (server.Registry), the catalog contains
// server definitions that can be discovered and installed.
package catalog

import (
	"math"
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

// BM25 field weights — name matches are strongest signal, then capabilities, then description.
const (
	bm25FieldName       = 3.0
	bm25FieldCapability = 2.0
	bm25FieldDesc       = 1.0

	bm25K1 = 1.2 // term frequency saturation
	bm25B  = 0.75
)

// Search finds entries matching the query using BM25 scoring.
// If query is empty, all entries are returned sorted alphabetically.
// If capability is provided, only entries with that capability are returned.
func (c *Catalog) Search(query, capability string) []*Entry {
	capability = strings.ToLower(strings.TrimSpace(capability))

	// Empty query → return all (filtered by capability), sorted alphabetically
	query = strings.TrimSpace(query)
	if query == "" {
		var results []*Entry
		for _, entry := range c.entries {
			if capability != "" && !entry.HasCapability(capability) {
				continue
			}
			results = append(results, entry)
		}
		sort.Slice(results, func(i, j int) bool {
			return results[i].Name < results[j].Name
		})
		return results
	}

	// BM25 scoring
	queryTerms := tokenize(query)

	// Pre-compute document frequencies across the catalog
	df := make(map[string]int) // term → number of entries containing it
	for _, entry := range c.entries {
		seen := make(map[string]bool)
		for _, term := range queryTerms {
			if _, ok := seen[term]; ok {
				continue
			}
			if fieldWeightedTF(entry, term) > 0 {
				seen[term] = true
				df[term]++
			}
		}
	}

	n := float64(len(c.entries))
	if n == 0 {
		return nil
	}

	type scored struct {
		entry *Entry
		score float64
	}

	var results []scored
	for _, entry := range c.entries {
		// Capability filter
		if capability != "" && !entry.HasCapability(capability) {
			continue
		}

		score := c.bm25Score(entry, queryTerms, df, n)
		if score > 0 {
			results = append(results, scored{entry: entry, score: score})
		}
	}

	// Sort by score descending; break ties by name ascending
	sort.Slice(results, func(i, j int) bool {
		if results[i].score != results[j].score {
			return results[i].score > results[j].score
		}
		return results[i].entry.Name < results[j].entry.Name
	})

	// Exact name match gets forced to top
	queryLower := strings.ToLower(query)
	out := make([]*Entry, 0, len(results))
	for _, r := range results {
		if strings.ToLower(r.entry.Name) == queryLower {
			out = append(out, r.entry)
			break
		}
	}
	for _, r := range results {
		if strings.ToLower(r.entry.Name) == queryLower {
			continue
		}
		out = append(out, r.entry)
	}

	return out
}

// bm25Score computes the BM25 score for an entry against query terms.
func (c *Catalog) bm25Score(entry *Entry, queryTerms []string, df map[string]int, n float64) float64 {
	var total float64

	// Average document length (total terms across all fields for all entries)
	var totalLen float64
	for _, e := range c.entries {
		totalLen += float64(len(entryTerms(e)))
	}
	avgDL := totalLen / n
	if avgDL == 0 {
		avgDL = 1
	}

	// Current document length
	docTerms := entryTerms(entry)
	dl := float64(len(docTerms))

	for _, term := range queryTerms {
		// Term frequency in this document (weighted by field)
		tf := fieldWeightedTF(entry, term)
		if tf == 0 {
			continue
		}

		docFreq := float64(df[term])
		// IDF with smoothing: log((N - df + 0.5) / (df + 0.5))
		// Floor at 0.01 so rare terms in tiny catalogs still contribute
		idf := math.Log((n - docFreq + 0.5) / (docFreq + 0.5))
		if idf < 0.01 {
			idf = 0.01
		}

		// BM25 TF normalization
		tfNorm := (tf * (bm25K1 + 1)) / (tf + bm25K1*(1-bm25B+bm25B*(dl/avgDL)))

		total += idf * tfNorm
	}

	return total
}

// fieldWeightedTF computes term frequency weighted by field importance.
// Supports partial (prefix) matches at reduced weight (0.5x) for better
// substring backward-compatibility.
func fieldWeightedTF(entry *Entry, term string) float64 {
	var tf float64

	// Name field (weight 3x)
	nameTerms := tokenize(entry.Name)
	for _, t := range nameTerms {
		tf += matchWeight(t, term, bm25FieldName)
	}

	// Capability fields (weight 2x)
	for _, cap := range entry.Capabilities {
		capTerms := tokenize(cap)
		for _, t := range capTerms {
			tf += matchWeight(t, term, bm25FieldCapability)
		}
	}

	// Description field (weight 1x)
	descTerms := tokenize(entry.Description)
	for _, t := range descTerms {
		tf += matchWeight(t, term, bm25FieldDesc)
	}

	return tf
}

// matchWeight returns full weight for exact token match, 0.5x for prefix match, 0 otherwise.
func matchWeight(token, query string, fieldWeight float64) float64 {
	if token == query {
		return fieldWeight
	}
	// Prefix match: query is a prefix of the token (e.g., "context" matches "context7")
	if len(query) < len(token) && strings.HasPrefix(token, query) {
		return fieldWeight * 0.5
	}
	return 0
}

// tokenize splits text into lowercase tokens on whitespace and hyphens.
func tokenize(text string) []string {
	text = strings.ToLower(text)
	// Replace hyphens with spaces for uniform splitting
	text = strings.ReplaceAll(text, "-", " ")
	fields := strings.Fields(text)
	return fields
}

// entryTerms returns all tokens from all fields of an entry.
func entryTerms(entry *Entry) []string {
	var terms []string
	terms = append(terms, tokenize(entry.Name)...)
	for _, cap := range entry.Capabilities {
		terms = append(terms, tokenize(cap)...)
	}
	terms = append(terms, tokenize(entry.Description)...)
	return terms
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

// GetTransport returns the transport type, defaulting to "stdio".
func (e *Entry) GetTransport() string {
	if e.Transport == "" {
		return "stdio"
	}
	return e.Transport
}
