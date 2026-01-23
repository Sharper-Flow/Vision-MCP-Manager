package catalog

// Default returns a catalog populated with well-known MCP servers.
// These are curated servers that are commonly used and well-tested.
func Default() *Catalog {
	c := New()

	// Documentation & Code Intelligence
	c.Add(&Entry{
		Name:        "context7",
		Description: "Library documentation lookup - query docs for any programming library",
		Capabilities: []string{
			"documentation",
			"library-lookup",
			"code-intelligence",
			"api-reference",
		},
		Command: "npx",
		Args:    []string{"-y", "@upstash/context7-mcp", "--api-key", "YOUR_API_KEY"},
		EnvVars: []string{"CONTEXT7_API_KEY"}, // Documents required key; user replaces YOUR_API_KEY in servers.yaml
		Source:  "https://github.com/upstash/context7",
	})

	// Web Scraping & Research
	c.Add(&Entry{
		Name:        "firecrawl",
		Description: "Web scraping and content extraction from URLs",
		Capabilities: []string{
			"web-scraping",
			"content-extraction",
			"research",
			"url-fetch",
		},
		Command: "npx",
		Args:    []string{"-y", "firecrawl-mcp"},
		EnvVars: []string{"FIRECRAWL_API_KEY"},
		Source:  "https://github.com/mendableai/firecrawl-mcp",
	})

	// Utilities
	c.Add(&Entry{
		Name:        "time",
		Description: "Current time and timezone conversion utilities",
		Capabilities: []string{
			"utility",
			"time",
			"timezone",
			"date",
		},
		Command: "npx",
		Args:    []string{"-y", "@anthropic/mcp-time"},
		Source:  "https://github.com/anthropics/mcp-servers",
	})

	c.Add(&Entry{
		Name:        "fetch",
		Description: "HTTP fetch utility for making web requests",
		Capabilities: []string{
			"utility",
			"http",
			"fetch",
			"web-request",
		},
		Command: "npx",
		Args:    []string{"-y", "@anthropic/mcp-fetch"},
		Source:  "https://github.com/anthropics/mcp-servers",
	})

	// File System
	c.Add(&Entry{
		Name:        "filesystem",
		Description: "File system operations with configurable allowed directories",
		Capabilities: []string{
			"filesystem",
			"file-read",
			"file-write",
			"directory",
		},
		Command: "npx",
		Args:    []string{"-y", "@anthropic/mcp-filesystem"},
		Source:  "https://github.com/anthropics/mcp-servers",
	})

	// Memory & Knowledge
	c.Add(&Entry{
		Name:        "basic-memory",
		Description: "Persistent memory storage for knowledge graphs and notes",
		Capabilities: []string{
			"memory",
			"knowledge-graph",
			"notes",
			"persistence",
		},
		Command: "uvx",
		Args:    []string{"basic-memory", "mcp"},
		Source:  "https://github.com/basicmachines-co/basic-memory",
	})

	// Search
	c.Add(&Entry{
		Name:        "kagi",
		Description: "Kagi search API for web search and summarization",
		Capabilities: []string{
			"search",
			"web-search",
			"summarization",
			"research",
		},
		Command: "uvx",
		Args:    []string{"kagimcp"},
		EnvVars: []string{"KAGI_API_KEY"},
		Source:  "https://github.com/nicholasgriffintn/kagimcp",
	})

	c.Add(&Entry{
		Name:        "arxiv",
		Description: "Search and download academic papers from arXiv",
		Capabilities: []string{
			"search",
			"academic",
			"papers",
			"research",
			"arxiv",
		},
		Command: "uvx",
		Args:    []string{"arxiv-mcp-server"},
		Source:  "https://github.com/blazickjp/arxiv-mcp-server",
	})

	// Database
	c.Add(&Entry{
		Name:        "postgres",
		Description: "PostgreSQL database operations and queries",
		Capabilities: []string{
			"database",
			"postgresql",
			"sql",
			"queries",
		},
		Command: "npx",
		Args:    []string{"-y", "@anthropic/mcp-postgres"},
		EnvVars: []string{"POSTGRES_CONNECTION_STRING"},
		Source:  "https://github.com/anthropics/mcp-servers",
	})

	c.Add(&Entry{
		Name:        "sqlite",
		Description: "SQLite database operations and queries",
		Capabilities: []string{
			"database",
			"sqlite",
			"sql",
			"queries",
		},
		Command: "npx",
		Args:    []string{"-y", "@anthropic/mcp-sqlite"},
		Source:  "https://github.com/anthropics/mcp-servers",
	})

	// Vector Database
	c.Add(&Entry{
		Name:        "qdrant",
		Description: "Qdrant vector database for semantic search and embeddings",
		Capabilities: []string{
			"database",
			"vector-db",
			"embeddings",
			"semantic-search",
		},
		Command: "uvx",
		Args:    []string{"mcp-server-qdrant"},
		EnvVars: []string{"QDRANT_URL", "QDRANT_API_KEY"},
		Source:  "https://github.com/qdrant/mcp-server-qdrant",
	})

	// Git & GitHub
	c.Add(&Entry{
		Name:        "github",
		Description: "GitHub API integration for repos, issues, PRs, and more",
		Capabilities: []string{
			"git",
			"github",
			"version-control",
			"issues",
			"pull-requests",
		},
		Command: "npx",
		Args:    []string{"-y", "@anthropic/mcp-github"},
		EnvVars: []string{"GITHUB_TOKEN"},
		Source:  "https://github.com/anthropics/mcp-servers",
	})

	// Code Search
	c.Add(&Entry{
		Name:        "grep-app",
		Description: "Search code across GitHub repositories using grep.app",
		Capabilities: []string{
			"code-search",
			"github",
			"grep",
			"search",
		},
		Command: "npx",
		Args:    []string{"-y", "@anthropic/mcp-grep-app"},
		Source:  "https://github.com/anthropics/mcp-servers",
	})

	// Browser Automation
	c.Add(&Entry{
		Name:        "puppeteer",
		Description: "Browser automation with Puppeteer for web interactions",
		Capabilities: []string{
			"browser",
			"automation",
			"puppeteer",
			"web-testing",
			"screenshots",
		},
		Command: "npx",
		Args:    []string{"-y", "@anthropic/mcp-puppeteer"},
		Source:  "https://github.com/anthropics/mcp-servers",
	})

	// Svelte
	c.Add(&Entry{
		Name:        "svelte",
		Description: "Svelte 5 and SvelteKit documentation and code assistance",
		Capabilities: []string{
			"documentation",
			"svelte",
			"sveltekit",
			"frontend",
			"code-intelligence",
		},
		Command: "npx",
		Args:    []string{"-y", "svelte-mcp"},
		Source:  "https://github.com/AshDevFr/svelte-mcp",
	})

	return c
}

// CapabilityCategories returns common capability categories for discovery.
func CapabilityCategories() map[string][]string {
	return map[string][]string{
		"documentation": {
			"context7", "svelte",
		},
		"web": {
			"firecrawl", "fetch", "puppeteer",
		},
		"search": {
			"kagi", "arxiv", "grep-app",
		},
		"database": {
			"postgres", "sqlite", "qdrant",
		},
		"utility": {
			"time", "fetch", "filesystem",
		},
		"memory": {
			"basic-memory",
		},
		"git": {
			"github",
		},
	}
}
