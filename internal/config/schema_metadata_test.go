package config

import (
	"strings"
	"testing"
)

// TestServerConfig_AcceptsSourceAndDescription verifies that the V2
// additions parse without error and round-trip cleanly through YAML.
// This is required for OpenCode Advance to pass source/description
// through stack.toml without stripping.
func TestServerConfig_AcceptsSourceAndDescription(t *testing.T) {
	yaml := `
servers:
  context7:
    port: 6276
    command: context7-mcp
    source: https://github.com/upstash/context7
    description: Library and API documentation lookup
`
	cfg, err := ParseYAML(yaml)
	if err != nil {
		t.Fatalf("ParseYAML failed: %v", err)
	}
	srv, ok := cfg.Servers["context7"]
	if !ok {
		t.Fatalf("servers.context7 missing")
	}
	if srv.Source != "https://github.com/upstash/context7" {
		t.Errorf("Source = %q, want github URL", srv.Source)
	}
	if !strings.Contains(srv.Description, "documentation lookup") {
		t.Errorf("Description = %q, want descriptive text", srv.Description)
	}
}

// TestServerConfig_SourceDescriptionOptional ensures omitting the fields
// still parses cleanly (backward compatibility).
func TestServerConfig_SourceDescriptionOptional(t *testing.T) {
	yaml := `
servers:
  minimal:
    port: 6277
    command: echo
`
	cfg, err := ParseYAML(yaml)
	if err != nil {
		t.Fatalf("ParseYAML failed: %v", err)
	}
	srv := cfg.Servers["minimal"]
	if srv.Source != "" {
		t.Errorf("Source should default empty, got %q", srv.Source)
	}
	if srv.Description != "" {
		t.Errorf("Description should default empty, got %q", srv.Description)
	}
}
