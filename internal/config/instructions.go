package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// InstructionsFile is the default filename for tool instructions
const InstructionsFile = "instructions.yaml"

var (
	// ErrInstructionsNotFound is returned when the instructions file doesn't exist
	ErrInstructionsNotFound = errors.New("instructions: file not found")
)

// Priority levels for tool/server guidance
const (
	PriorityHigh   = "high"
	PriorityMedium = "medium"
	PriorityLow    = "low"
)

// ServerInstructions defines guidance for an MCP server.
type ServerInstructions struct {
	// Priority indicates how preferred this server is (high, medium, low)
	Priority string `yaml:"priority,omitempty" json:"priority,omitempty"`

	// Guidance is extended usage instructions for AI agents
	Guidance string `yaml:"guidance,omitempty" json:"guidance,omitempty"`

	// PreferFor lists use cases where this server should be preferred
	PreferFor []string `yaml:"prefer_for,omitempty" json:"prefer_for,omitempty"`

	// AvoidFor lists use cases where this server should NOT be used
	AvoidFor []string `yaml:"avoid_for,omitempty" json:"avoid_for,omitempty"`

	// Examples are usage examples to help agents understand correct usage
	Examples []string `yaml:"examples,omitempty" json:"examples,omitempty"`
}

// ToolInstructions defines guidance for a specific tool within a server.
type ToolInstructions struct {
	// Priority indicates how preferred this tool is (high, medium, low)
	Priority string `yaml:"priority,omitempty" json:"priority,omitempty"`

	// Guidance is extended usage instructions for AI agents
	Guidance string `yaml:"guidance,omitempty" json:"guidance,omitempty"`

	// PreferFor lists use cases where this tool should be preferred
	PreferFor []string `yaml:"prefer_for,omitempty" json:"prefer_for,omitempty"`

	// AvoidFor lists use cases where this tool should NOT be used
	AvoidFor []string `yaml:"avoid_for,omitempty" json:"avoid_for,omitempty"`

	// Examples are usage examples to help agents understand correct usage
	Examples []string `yaml:"examples,omitempty" json:"examples,omitempty"`
}

// Instructions is the root structure for tool/server guidance configuration.
type Instructions struct {
	// Servers maps server names to their instructions
	Servers map[string]*ServerInstructions `yaml:"servers,omitempty" json:"servers,omitempty"`

	// Tools maps tool names to their instructions (for cross-server tool guidance)
	Tools map[string]*ToolInstructions `yaml:"tools,omitempty" json:"tools,omitempty"`

	// GlobalGuidance is general guidance that applies to all tool selection
	GlobalGuidance string `yaml:"global_guidance,omitempty" json:"global_guidance,omitempty"`
}

// DefaultInstructionsPath returns the default instructions file path: ~/.config/vision/instructions.yaml
func DefaultInstructionsPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return InstructionsFile
	}
	return filepath.Join(home, DefaultConfigDir, InstructionsFile)
}

// LoadInstructions reads and parses an instructions file from the given path.
// If path is empty, it uses DefaultInstructionsPath().
// Returns an empty Instructions struct (not nil) if file doesn't exist.
func LoadInstructions(path string) (*Instructions, error) {
	if path == "" {
		path = DefaultInstructionsPath()
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			// Return empty instructions if file doesn't exist (not an error)
			return &Instructions{
				Servers: make(map[string]*ServerInstructions),
				Tools:   make(map[string]*ToolInstructions),
			}, nil
		}
		return nil, fmt.Errorf("instructions: read error: %w", err)
	}

	// Expand environment variables
	expanded := ExpandEnvVars(string(data))

	var inst Instructions
	if err := yaml.Unmarshal([]byte(expanded), &inst); err != nil {
		return nil, fmt.Errorf("instructions: parse error: %w", err)
	}

	// Initialize maps if nil
	if inst.Servers == nil {
		inst.Servers = make(map[string]*ServerInstructions)
	}
	if inst.Tools == nil {
		inst.Tools = make(map[string]*ToolInstructions)
	}

	return &inst, nil
}

// GetServerInstructions returns instructions for a specific server, or nil if not found.
func (i *Instructions) GetServerInstructions(name string) *ServerInstructions {
	if i == nil || i.Servers == nil {
		return nil
	}
	return i.Servers[name]
}

// GetToolInstructions returns instructions for a specific tool, or nil if not found.
func (i *Instructions) GetToolInstructions(name string) *ToolInstructions {
	if i == nil || i.Tools == nil {
		return nil
	}
	return i.Tools[name]
}

// HasInstructions returns true if any instructions are configured.
func (i *Instructions) HasInstructions() bool {
	if i == nil {
		return false
	}
	return len(i.Servers) > 0 || len(i.Tools) > 0 || i.GlobalGuidance != ""
}

// ServerNames returns a sorted list of server names with instructions.
func (i *Instructions) ServerNames() []string {
	if i == nil || i.Servers == nil {
		return nil
	}
	names := make([]string, 0, len(i.Servers))
	for name := range i.Servers {
		names = append(names, name)
	}
	sortStrings(names)
	return names
}

// ToolNames returns a sorted list of tool names with instructions.
func (i *Instructions) ToolNames() []string {
	if i == nil || i.Tools == nil {
		return nil
	}
	names := make([]string, 0, len(i.Tools))
	for name := range i.Tools {
		names = append(names, name)
	}
	sortStrings(names)
	return names
}
