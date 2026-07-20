package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadInstructions_FileNotFound(t *testing.T) {
	// Should return empty instructions (not error) when file doesn't exist
	inst, err := LoadInstructions("/nonexistent/path/to/instructions.yaml")
	if err != nil {
		t.Fatalf("LoadInstructions() unexpected error for nonexistent file: %v", err)
	}
	if inst == nil {
		t.Fatal("LoadInstructions() returned nil")
	}
	if inst.HasInstructions() {
		t.Error("Empty instructions should return false for HasInstructions()")
	}
}

func TestLoadInstructions_ValidFile(t *testing.T) {
	// Create temp file with valid instructions
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "instructions.yaml")

	yaml := `
global_guidance: "Test global guidance"
servers:
  kagi:
    priority: high
    guidance: "Use for web search"
    prefer_for:
      - web search
      - research
    avoid_for:
      - documentation
    examples:
      - "kagi_search_fetch queries=['test']"
  playwright:
    priority: low
    guidance: "Only for browser automation"
    avoid_for:
      - web search
      - research
tools:
  custom_tool:
    priority: medium
    guidance: "Custom tool guidance"
    namespaced_name: "tools.custom.lookup"
`
	if err := os.WriteFile(path, []byte(yaml), 0644); err != nil {
		t.Fatalf("Failed to write test file: %v", err)
	}

	inst, err := LoadInstructions(path)
	if err != nil {
		t.Fatalf("LoadInstructions() error: %v", err)
	}

	// Check global guidance
	if inst.GlobalGuidance != "Test global guidance" {
		t.Errorf("GlobalGuidance = %q, want %q", inst.GlobalGuidance, "Test global guidance")
	}

	// Check HasInstructions
	if !inst.HasInstructions() {
		t.Error("HasInstructions() should return true")
	}

	// Check server instructions
	kagi := inst.GetServerInstructions("kagi")
	if kagi == nil {
		t.Fatal("GetServerInstructions('kagi') returned nil")
	}
	if kagi.Priority != "high" {
		t.Errorf("kagi.Priority = %q, want %q", kagi.Priority, "high")
	}
	if kagi.Guidance != "Use for web search" {
		t.Errorf("kagi.Guidance = %q, want %q", kagi.Guidance, "Use for web search")
	}
	if len(kagi.PreferFor) != 2 {
		t.Errorf("len(kagi.PreferFor) = %d, want 2", len(kagi.PreferFor))
	}
	if len(kagi.AvoidFor) != 1 {
		t.Errorf("len(kagi.AvoidFor) = %d, want 1", len(kagi.AvoidFor))
	}
	if len(kagi.Examples) != 1 {
		t.Errorf("len(kagi.Examples) = %d, want 1", len(kagi.Examples))
	}

	// Check playwright
	playwright := inst.GetServerInstructions("playwright")
	if playwright == nil {
		t.Fatal("GetServerInstructions('playwright') returned nil")
	}
	if playwright.Priority != "low" {
		t.Errorf("playwright.Priority = %q, want %q", playwright.Priority, "low")
	}

	// Check tool instructions
	tool := inst.GetToolInstructions("custom_tool")
	if tool == nil {
		t.Fatal("GetToolInstructions('custom_tool') returned nil")
	}
	if tool.Priority != "medium" {
		t.Errorf("tool.Priority = %q, want %q", tool.Priority, "medium")
	}
	if tool.NamespacedName != "tools.custom.lookup" {
		t.Errorf("tool.NamespacedName = %q, want %q", tool.NamespacedName, "tools.custom.lookup")
	}

	// Check nonexistent
	if inst.GetServerInstructions("nonexistent") != nil {
		t.Error("GetServerInstructions('nonexistent') should return nil")
	}
	if inst.GetToolInstructions("nonexistent") != nil {
		t.Error("GetToolInstructions('nonexistent') should return nil")
	}
}

func TestLoadInstructions_InvalidYAML(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "invalid.yaml")

	// Write invalid YAML
	if err := os.WriteFile(path, []byte("invalid: yaml: content: ["), 0644); err != nil {
		t.Fatalf("Failed to write test file: %v", err)
	}

	_, err := LoadInstructions(path)
	if err == nil {
		t.Fatal("LoadInstructions() should error on invalid YAML")
	}
}

func TestInstructions_ServerNames(t *testing.T) {
	inst := &Instructions{
		Servers: map[string]*ServerInstructions{
			"zebra": {Priority: "low"},
			"alpha": {Priority: "high"},
			"beta":  {Priority: "medium"},
		},
	}

	names := inst.ServerNames()
	if len(names) != 3 {
		t.Fatalf("Expected 3 names, got %d", len(names))
	}

	// Should be sorted
	expected := []string{"alpha", "beta", "zebra"}
	for i, name := range names {
		if name != expected[i] {
			t.Errorf("names[%d] = %q, want %q", i, name, expected[i])
		}
	}
}

func TestInstructions_ToolNames(t *testing.T) {
	inst := &Instructions{
		Tools: map[string]*ToolInstructions{
			"tool_c": {Priority: "low"},
			"tool_a": {Priority: "high"},
			"tool_b": {Priority: "medium"},
		},
	}

	names := inst.ToolNames()
	if len(names) != 3 {
		t.Fatalf("Expected 3 names, got %d", len(names))
	}

	// Should be sorted
	expected := []string{"tool_a", "tool_b", "tool_c"}
	for i, name := range names {
		if name != expected[i] {
			t.Errorf("names[%d] = %q, want %q", i, name, expected[i])
		}
	}
}

func TestInstructions_NilSafe(t *testing.T) {
	var inst *Instructions

	// All methods should be nil-safe
	if inst.GetServerInstructions("test") != nil {
		t.Error("nil Instructions.GetServerInstructions should return nil")
	}
	if inst.GetToolInstructions("test") != nil {
		t.Error("nil Instructions.GetToolInstructions should return nil")
	}
	if inst.HasInstructions() {
		t.Error("nil Instructions.HasInstructions should return false")
	}
	if inst.ServerNames() != nil {
		t.Error("nil Instructions.ServerNames should return nil")
	}
	if inst.ToolNames() != nil {
		t.Error("nil Instructions.ToolNames should return nil")
	}
}

func TestLoadInstructions_EnvironmentExpansion(t *testing.T) {
	os.Setenv("TEST_PRIORITY", "high")
	defer os.Unsetenv("TEST_PRIORITY")

	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "instructions.yaml")

	yaml := `
servers:
  test:
    priority: ${TEST_PRIORITY}
    guidance: "Test guidance"
`
	if err := os.WriteFile(path, []byte(yaml), 0644); err != nil {
		t.Fatalf("Failed to write test file: %v", err)
	}

	inst, err := LoadInstructions(path)
	if err != nil {
		t.Fatalf("LoadInstructions() error: %v", err)
	}

	test := inst.GetServerInstructions("test")
	if test == nil {
		t.Fatal("GetServerInstructions('test') returned nil")
	}
	if test.Priority != "high" {
		t.Errorf("Priority = %q, want %q (from env)", test.Priority, "high")
	}
}

func TestDefaultInstructionsPath(t *testing.T) {
	path := DefaultInstructionsPath()
	if path == "" {
		t.Error("DefaultInstructionsPath() returned empty string")
	}

	// Should end with instructions.yaml
	if filepath.Base(path) != InstructionsFile {
		t.Errorf("DefaultInstructionsPath() should end with %q, got %q", InstructionsFile, filepath.Base(path))
	}
}
