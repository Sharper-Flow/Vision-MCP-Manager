package admin

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jrede/vision/internal/config"
)

// TestToolSlotStatus_ReturnsGroupWithSlots verifies the response shape:
// {groups: [{group_name, slot_count, slots: [{name, port, active_sessions, max_sessions}]}]}
func TestToolSlotStatus_ReturnsGroupWithSlots(t *testing.T) {
	reg := newTestRegistry()

	dcfg := &config.Config{
		Servers: map[string]*config.ServerConfig{
			"pw-slot-1": {Port: 7001, Command: "echo", SlotGroup: "playwright", SlotIndex: 1, MaxSessions: 5},
			"pw-slot-2": {Port: 7002, Command: "echo", SlotGroup: "playwright", SlotIndex: 2, MaxSessions: 5},
		},
		SlotGroups: map[string]*config.SlotGroupConfig{
			"playwright": {Template: "pw-slot", BasePort: 7001, Count: 2, GroupPort: 7000},
		},
	}

	s := &Server{registry: reg, daemonConfig: dcfg}

	result, err := s.toolSlotStatus(context.TODO(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("toolSlotStatus error: %v", err)
	}
	if len(result.Content) == 0 {
		t.Fatal("toolSlotStatus returned no content")
	}

	var resp SlotStatusResponse
	if err := json.Unmarshal([]byte(result.Content[0].Text), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	if len(resp.Groups) != 1 {
		t.Fatalf("groups count = %d, want 1", len(resp.Groups))
	}

	g := resp.Groups[0]
	if g.GroupName != "playwright" {
		t.Errorf("group_name = %q, want %q", g.GroupName, "playwright")
	}
	if g.SlotCount != 2 {
		t.Errorf("slot_count = %d, want 2", g.SlotCount)
	}
	if len(g.Slots) != 2 {
		t.Fatalf("slots count = %d, want 2", len(g.Slots))
	}

	// Slots should be sorted by name.
	s1 := g.Slots[0]
	if s1.Name != "pw-slot-1" {
		t.Errorf("slot[0].name = %q, want %q", s1.Name, "pw-slot-1")
	}
	if s1.Port != 7001 {
		t.Errorf("slot[0].port = %d, want 7001", s1.Port)
	}
	// active_sessions = 0 when no session accessor wired.
	if s1.ActiveSessions != 0 {
		t.Errorf("slot[0].active_sessions = %d, want 0", s1.ActiveSessions)
	}
	if s1.MaxSessions != 5 {
		t.Errorf("slot[0].max_sessions = %d, want 5", s1.MaxSessions)
	}

	s2 := g.Slots[1]
	if s2.Name != "pw-slot-2" {
		t.Errorf("slot[1].name = %q, want %q", s2.Name, "pw-slot-2")
	}
	if s2.Port != 7002 {
		t.Errorf("slot[1].port = %d, want 7002", s2.Port)
	}
}

// TestToolSlotStatus_EmptyWhenNoSlotGroups returns empty groups array
// (not nil) when daemonConfig has no slot groups.
func TestToolSlotStatus_EmptyWhenNoSlotGroups(t *testing.T) {
	reg := newTestRegistry()
	dcfg := &config.Config{
		Servers:    map[string]*config.ServerConfig{},
		SlotGroups: map[string]*config.SlotGroupConfig{},
	}
	s := &Server{registry: reg, daemonConfig: dcfg}

	result, err := s.toolSlotStatus(context.TODO(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("toolSlotStatus error: %v", err)
	}

	var resp SlotStatusResponse
	if err := json.Unmarshal([]byte(result.Content[0].Text), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	if resp.Groups == nil {
		t.Fatal("groups is nil — should be empty array, not null")
	}
	if len(resp.Groups) != 0 {
		t.Fatalf("groups count = %d, want 0", len(resp.Groups))
	}
}

// TestToolSlotStatus_NilDaemonConfig returns empty groups when daemonConfig
// is nil (backward compat).
func TestToolSlotStatus_NilDaemonConfig(t *testing.T) {
	reg := newTestRegistry()
	s := &Server{registry: reg, daemonConfig: nil}

	result, err := s.toolSlotStatus(context.TODO(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("toolSlotStatus error: %v", err)
	}

	var resp SlotStatusResponse
	if err := json.Unmarshal([]byte(result.Content[0].Text), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	if len(resp.Groups) != 0 {
		t.Fatalf("groups count = %d, want 0 when daemonConfig nil", len(resp.Groups))
	}
}

// TestToolSlotStatus_WithSessionAccessor verifies that active_sessions
// is populated when a SlotSessionAccessor is provided.
func TestToolSlotStatus_WithSessionAccessor(t *testing.T) {
	reg := newTestRegistry()
	dcfg := &config.Config{
		Servers: map[string]*config.ServerConfig{
			"pw-slot-1": {Port: 7001, Command: "echo", SlotGroup: "playwright", SlotIndex: 1, MaxSessions: 3},
		},
		SlotGroups: map[string]*config.SlotGroupConfig{
			"playwright": {Template: "pw-slot", BasePort: 7001, Count: 1, GroupPort: 7000},
		},
	}

	accessor := &stubSessionAccessor{
		counts: map[string]int{"pw-slot-1": 2},
	}
	s := &Server{registry: reg, daemonConfig: dcfg, slotSessionAccessor: accessor}

	result, err := s.toolSlotStatus(context.TODO(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("toolSlotStatus error: %v", err)
	}

	var resp SlotStatusResponse
	if err := json.Unmarshal([]byte(result.Content[0].Text), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	if len(resp.Groups) != 1 || len(resp.Groups[0].Slots) != 1 {
		t.Fatal("expected 1 group with 1 slot")
	}
	slot := resp.Groups[0].Slots[0]
	if slot.ActiveSessions != 2 {
		t.Errorf("active_sessions = %d, want 2", slot.ActiveSessions)
	}
}

// TestToolSlotStatus_MultipleGroupsSorted verifies groups are sorted by name.
func TestToolSlotStatus_MultipleGroupsSorted(t *testing.T) {
	reg := newTestRegistry()
	dcfg := &config.Config{
		Servers: map[string]*config.ServerConfig{
			"beta-1":  {Port: 6001, Command: "echo", SlotGroup: "beta", SlotIndex: 1},
			"alpha-1": {Port: 5001, Command: "echo", SlotGroup: "alpha", SlotIndex: 1},
		},
		SlotGroups: map[string]*config.SlotGroupConfig{
			"beta":  {Template: "beta", BasePort: 6001, Count: 1, GroupPort: 6000},
			"alpha": {Template: "alpha", BasePort: 5001, Count: 1, GroupPort: 5000},
		},
	}

	s := &Server{registry: reg, daemonConfig: dcfg}
	result, err := s.toolSlotStatus(context.TODO(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("toolSlotStatus error: %v", err)
	}

	var resp SlotStatusResponse
	if err := json.Unmarshal([]byte(result.Content[0].Text), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	if len(resp.Groups) != 2 {
		t.Fatalf("groups count = %d, want 2", len(resp.Groups))
	}
	if resp.Groups[0].GroupName != "alpha" {
		t.Errorf("groups[0].group_name = %q, want %q", resp.Groups[0].GroupName, "alpha")
	}
	if resp.Groups[1].GroupName != "beta" {
		t.Errorf("groups[1].group_name = %q, want %q", resp.Groups[1].GroupName, "beta")
	}
}

// --- test helpers ---

// stubSessionAccessor is a test-only SlotSessionAccessor.
type stubSessionAccessor struct {
	counts map[string]int
}

func (s *stubSessionAccessor) ActiveSessionCount(slotName string) int {
	return s.counts[slotName]
}
