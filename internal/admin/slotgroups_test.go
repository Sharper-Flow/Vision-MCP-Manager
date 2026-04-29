package admin

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jrede/vision/internal/config"
)

// TestToolList_IncludesSlotGroups verifies that vision_list adds a slot_groups
// section when daemonConfig contains slot group definitions.  The existing
// servers array must remain unchanged (additive-only per acceptance criterion 4).
func TestToolList_IncludesSlotGroups(t *testing.T) {
	reg := newTestRegistry()

	dcfg := &config.Config{
		Servers: map[string]*config.ServerConfig{
			"alpha-1": {Port: 5001, SlotGroup: "alpha", SlotIndex: 1},
			"alpha-2": {Port: 5002, SlotGroup: "alpha", SlotIndex: 2},
			"beta-1":  {Port: 6001, SlotGroup: "beta", SlotIndex: 1},
			"beta-2":  {Port: 6002, SlotGroup: "beta", SlotIndex: 2},
		},
		SlotGroups: map[string]*config.SlotGroupConfig{
			"alpha": {Template: "alpha", BasePort: 5001, Count: 2, GroupPort: 5000},
			"beta":  {Template: "beta", BasePort: 6001, Count: 2, GroupPort: 6000},
		},
	}

	s := &Server{registry: reg, daemonConfig: dcfg}

	result, err := s.toolList(context.TODO(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("toolList error: %v", err)
	}
	if len(result.Content) == 0 {
		t.Fatal("toolList returned no content")
	}

	var resp ListResponse
	if err := json.Unmarshal([]byte(result.Content[0].Text), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	// Existing servers array must still exist.
	if resp.Servers == nil {
		t.Fatal("servers field is nil — must preserve existing shape")
	}

	// New slot_groups section must be present.
	if len(resp.SlotGroups) == 0 {
		t.Fatal("slot_groups is empty — expected 2 groups")
	}
	if len(resp.SlotGroups) != 2 {
		t.Fatalf("slot_groups count = %d, want 2", len(resp.SlotGroups))
	}

	// Verify alpha group.
	alpha := findSlotGroup(t, resp.SlotGroups, "alpha")
	if alpha.GroupPort != 5000 {
		t.Errorf("alpha.GroupPort = %d, want 5000", alpha.GroupPort)
	}
	assertSlotMembers(t, alpha.Slots, []string{"alpha-1", "alpha-2"})

	// Verify beta group.
	beta := findSlotGroup(t, resp.SlotGroups, "beta")
	if beta.GroupPort != 6000 {
		t.Errorf("beta.GroupPort = %d, want 6000", beta.GroupPort)
	}
	assertSlotMembers(t, beta.Slots, []string{"beta-1", "beta-2"})
}

// TestToolList_SlotGroupsEmptyWhenNoneConfigured verifies that slot_groups
// is an empty array (not nil) when daemonConfig has no slot groups.
func TestToolList_SlotGroupsEmptyWhenNoneConfigured(t *testing.T) {
	reg := newTestRegistry()

	dcfg := &config.Config{
		Servers:    map[string]*config.ServerConfig{},
		SlotGroups: map[string]*config.SlotGroupConfig{},
	}

	s := &Server{registry: reg, daemonConfig: dcfg}

	result, err := s.toolList(context.TODO(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("toolList error: %v", err)
	}

	var resp ListResponse
	if err := json.Unmarshal([]byte(result.Content[0].Text), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	if resp.SlotGroups == nil {
		t.Fatal("slot_groups is nil — should be empty array, not null")
	}
	if len(resp.SlotGroups) != 0 {
		t.Fatalf("slot_groups count = %d, want 0", len(resp.SlotGroups))
	}
}

// TestToolList_SlotGroupsNilDaemonConfig verifies that vision_list works
// when daemonConfig is nil (backward compat).
func TestToolList_SlotGroupsNilDaemonConfig(t *testing.T) {
	reg := newTestRegistry()
	s := &Server{registry: reg, daemonConfig: nil}

	result, err := s.toolList(context.TODO(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("toolList error: %v", err)
	}

	var resp ListResponse
	if err := json.Unmarshal([]byte(result.Content[0].Text), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	if resp.Servers == nil {
		t.Fatal("servers field is nil")
	}
	// slot_groups should be empty, not crash.
	if len(resp.SlotGroups) != 0 {
		t.Fatalf("slot_groups count = %d, want 0 when daemonConfig nil", len(resp.SlotGroups))
	}
}

// --- helpers ---

func findSlotGroup(t *testing.T, groups []SlotGroupEntry, name string) SlotGroupEntry {
	t.Helper()
	for _, g := range groups {
		if g.Name == name {
			return g
		}
	}
	t.Fatalf("slot group %q not found", name)
	return SlotGroupEntry{}
}

func assertSlotMembers(t *testing.T, got []string, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("slot members = %v, want %v", got, want)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("slot member[%d] = %q, want %q", i, got[i], w)
		}
	}
}
