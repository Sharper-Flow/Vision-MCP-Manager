package catalog

import (
	"testing"
)

func TestCatalog_AddAndGet(t *testing.T) {
	c := New()

	entry := &Entry{
		Name:         "test-server",
		Description:  "A test server",
		Capabilities: []string{"testing", "example"},
		Command:      "npx",
		Args:         []string{"-y", "test-mcp"},
	}

	c.Add(entry)

	got := c.Get("test-server")
	if got == nil {
		t.Fatal("expected to get entry, got nil")
	}
	if got.Name != "test-server" {
		t.Errorf("got name %q, want %q", got.Name, "test-server")
	}
}

func TestCatalog_GetNotFound(t *testing.T) {
	c := New()

	got := c.Get("nonexistent")
	if got != nil {
		t.Errorf("expected nil for nonexistent entry, got %v", got)
	}
}

func TestCatalog_List(t *testing.T) {
	c := New()

	c.Add(&Entry{Name: "zebra", Description: "Last"})
	c.Add(&Entry{Name: "alpha", Description: "First"})
	c.Add(&Entry{Name: "beta", Description: "Middle"})

	list := c.List()
	if len(list) != 3 {
		t.Fatalf("got %d entries, want 3", len(list))
	}

	// Should be sorted by name
	if list[0].Name != "alpha" {
		t.Errorf("first entry should be alpha, got %s", list[0].Name)
	}
	if list[1].Name != "beta" {
		t.Errorf("second entry should be beta, got %s", list[1].Name)
	}
	if list[2].Name != "zebra" {
		t.Errorf("third entry should be zebra, got %s", list[2].Name)
	}
}

func TestCatalog_SearchByName(t *testing.T) {
	c := New()

	c.Add(&Entry{Name: "context7", Description: "Library docs"})
	c.Add(&Entry{Name: "firecrawl", Description: "Web scraping"})
	c.Add(&Entry{Name: "time", Description: "Time utilities"})

	results := c.Search("context", "")
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	if results[0].Name != "context7" {
		t.Errorf("got %s, want context7", results[0].Name)
	}
}

func TestCatalog_SearchByDescription(t *testing.T) {
	c := New()

	c.Add(&Entry{Name: "context7", Description: "Library documentation lookup"})
	c.Add(&Entry{Name: "firecrawl", Description: "Web scraping"})

	results := c.Search("documentation", "")
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	if results[0].Name != "context7" {
		t.Errorf("got %s, want context7", results[0].Name)
	}
}

func TestCatalog_SearchByCapability(t *testing.T) {
	c := New()

	c.Add(&Entry{
		Name:         "context7",
		Description:  "Library docs",
		Capabilities: []string{"documentation", "code-intelligence"},
	})
	c.Add(&Entry{
		Name:         "firecrawl",
		Description:  "Web scraping",
		Capabilities: []string{"web-scraping", "research"},
	})

	// Search by capability filter only
	results := c.Search("", "documentation")
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	if results[0].Name != "context7" {
		t.Errorf("got %s, want context7", results[0].Name)
	}
}

func TestCatalog_SearchCapabilityInQuery(t *testing.T) {
	c := New()

	c.Add(&Entry{
		Name:         "context7",
		Description:  "Library docs",
		Capabilities: []string{"documentation", "code-intelligence"},
	})
	c.Add(&Entry{
		Name:         "firecrawl",
		Description:  "Web scraping",
		Capabilities: []string{"web-scraping", "research"},
	})

	// Search with query matching a capability
	results := c.Search("web-scraping", "")
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	if results[0].Name != "firecrawl" {
		t.Errorf("got %s, want firecrawl", results[0].Name)
	}
}

func TestCatalog_SearchCaseInsensitive(t *testing.T) {
	c := New()

	c.Add(&Entry{Name: "Context7", Description: "Library docs"})

	results := c.Search("CONTEXT", "")
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
}

func TestCatalog_SearchNoResults(t *testing.T) {
	c := New()

	c.Add(&Entry{Name: "context7", Description: "Library docs"})

	results := c.Search("nonexistent", "")
	if len(results) != 0 {
		t.Errorf("got %d results, want 0", len(results))
	}
}

func TestCatalog_SearchEmpty(t *testing.T) {
	c := New()

	c.Add(&Entry{Name: "a", Description: "First"})
	c.Add(&Entry{Name: "b", Description: "Second"})

	// Empty query should return all
	results := c.Search("", "")
	if len(results) != 2 {
		t.Errorf("got %d results, want 2", len(results))
	}
}

func TestCatalog_SearchExactMatchFirst(t *testing.T) {
	c := New()

	c.Add(&Entry{Name: "time", Description: "Time utils"})
	c.Add(&Entry{Name: "timeout", Description: "Timeout helper"})
	c.Add(&Entry{Name: "realtime", Description: "Real-time sync"})

	results := c.Search("time", "")
	if len(results) != 3 {
		t.Fatalf("got %d results, want 3", len(results))
	}
	// Exact match should be first
	if results[0].Name != "time" {
		t.Errorf("exact match should be first, got %s", results[0].Name)
	}
}

func TestEntry_HasCapability(t *testing.T) {
	e := &Entry{
		Capabilities: []string{"Documentation", "Code-Intelligence"},
	}

	// Case insensitive
	if !e.HasCapability("documentation") {
		t.Error("should have documentation capability")
	}
	if !e.HasCapability("DOCUMENTATION") {
		t.Error("should have DOCUMENTATION capability (case insensitive)")
	}
	if e.HasCapability("nonexistent") {
		t.Error("should not have nonexistent capability")
	}
}

func TestEntry_GetTransport(t *testing.T) {
	tests := []struct {
		transport string
		want      string
	}{
		{"", "stdio"},
		{"stdio", "stdio"},
		{"http", "http"},
		{"sse", "sse"},
	}

	for _, tt := range tests {
		e := &Entry{Transport: tt.transport}
		if got := e.GetTransport(); got != tt.want {
			t.Errorf("GetTransport() with %q = %q, want %q", tt.transport, got, tt.want)
		}
	}
}

func TestDefault(t *testing.T) {
	c := Default()

	// Should have entries
	if c.Count() == 0 {
		t.Fatal("default catalog should have entries")
	}

	// Check some expected servers exist
	expected := []string{"context7", "firecrawl", "time", "fetch"}
	for _, name := range expected {
		if c.Get(name) == nil {
			t.Errorf("expected %s in default catalog", name)
		}
	}

	// Check context7 has expected capabilities
	context7 := c.Get("context7")
	if context7 == nil {
		t.Fatal("context7 not found")
	}
	if !context7.HasCapability("documentation") {
		t.Error("context7 should have documentation capability")
	}
}

func TestCatalog_AddNil(t *testing.T) {
	c := New()

	// Should not panic
	c.Add(nil)
	c.Add(&Entry{}) // Empty name

	if c.Count() != 0 {
		t.Error("nil and empty entries should not be added")
	}
}
