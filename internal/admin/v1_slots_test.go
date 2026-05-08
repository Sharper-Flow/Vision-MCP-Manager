package admin

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/config"
)

// --- handleV1Slots (GET /v1/slots) tests ---

// TestHandleV1Slots_ReturnsAllGroupsSummary verifies GET /v1/slots returns
// a JSON envelope {groups: [{group_name, slot_count, slots: [...]}]} with
// all groups sorted by name.
func TestHandleV1Slots_ReturnsAllGroupsSummary(t *testing.T) {
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
	s := &Server{registry: reg, daemonConfig: dcfg, running: true}

	req := httptest.NewRequest(http.MethodGet, "/v1/slots", nil)
	rr := httptest.NewRecorder()
	s.handleV1Slots(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var body struct {
		Groups []SlotGroupStatus `json:"groups"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v; body=%s", err, rr.Body.String())
	}
	if len(body.Groups) != 1 {
		t.Fatalf("groups count = %d, want 1", len(body.Groups))
	}
	g := body.Groups[0]
	if g.GroupName != "playwright" {
		t.Errorf("group_name = %q, want %q", g.GroupName, "playwright")
	}
	if g.SlotCount != 2 {
		t.Errorf("slot_count = %d, want 2", g.SlotCount)
	}
	if len(g.Slots) != 2 {
		t.Fatalf("slots count = %d, want 2", len(g.Slots))
	}
}

// TestHandleV1Slots_EmptyGroupsReturnsEmptyArray verifies empty groups
// returns [] not null.
func TestHandleV1Slots_EmptyGroupsReturnsEmptyArray(t *testing.T) {
	reg := newTestRegistry()
	dcfg := &config.Config{
		Servers:    map[string]*config.ServerConfig{},
		SlotGroups: map[string]*config.SlotGroupConfig{},
	}
	s := &Server{registry: reg, daemonConfig: dcfg, running: true}

	req := httptest.NewRequest(http.MethodGet, "/v1/slots", nil)
	rr := httptest.NewRecorder()
	s.handleV1Slots(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}

	var body struct {
		Groups []SlotGroupStatus `json:"groups"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Groups == nil {
		t.Fatal("groups is nil — should be empty array, not null")
	}
	if len(body.Groups) != 0 {
		t.Fatalf("groups count = %d, want 0", len(body.Groups))
	}
}

// TestHandleV1Slots_NotRunningReturns503 mirrors v1_servers behavior.
func TestHandleV1Slots_NotRunningReturns503(t *testing.T) {
	s := &Server{running: false}
	req := httptest.NewRequest(http.MethodGet, "/v1/slots", nil)
	rr := httptest.NewRecorder()
	s.handleV1Slots(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rr.Code)
	}
}

// --- handleV1SlotsGroup (GET /v1/slots/{group}) tests ---

// TestHandleV1SlotsGroup_ReturnsSingleGroup verifies GET /v1/slots/{group}
// returns detailed status for one group.
func TestHandleV1SlotsGroup_ReturnsSingleGroup(t *testing.T) {
	reg := newTestRegistry()
	dcfg := &config.Config{
		Servers: map[string]*config.ServerConfig{
			"pw-slot-1": {Port: 7001, Command: "echo", SlotGroup: "playwright", SlotIndex: 1, MaxSessions: 3},
		},
		SlotGroups: map[string]*config.SlotGroupConfig{
			"playwright": {Template: "pw-slot", BasePort: 7001, Count: 1, GroupPort: 7000},
		},
	}
	s := &Server{registry: reg, daemonConfig: dcfg, running: true}

	req := httptest.NewRequest(http.MethodGet, "/v1/slots/playwright", nil)
	req.SetPathValue("group", "playwright")
	rr := httptest.NewRecorder()
	s.handleV1SlotsGroup(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}

	var g SlotGroupStatus
	if err := json.Unmarshal(rr.Body.Bytes(), &g); err != nil {
		t.Fatalf("unmarshal: %v; body=%s", err, rr.Body.String())
	}
	if g.GroupName != "playwright" {
		t.Errorf("group_name = %q, want %q", g.GroupName, "playwright")
	}
	if g.SlotCount != 1 {
		t.Errorf("slot_count = %d, want 1", g.SlotCount)
	}
	if len(g.Slots) != 1 {
		t.Fatalf("slots count = %d, want 1", len(g.Slots))
	}
	if g.Slots[0].Name != "pw-slot-1" {
		t.Errorf("slot[0].name = %q, want %q", g.Slots[0].Name, "pw-slot-1")
	}
	if g.Slots[0].Port != 7001 {
		t.Errorf("slot[0].port = %d, want 7001", g.Slots[0].Port)
	}
	if g.Slots[0].MaxSessions != 3 {
		t.Errorf("slot[0].max_sessions = %d, want 3", g.Slots[0].MaxSessions)
	}
}

// TestHandleV1SlotsGroup_NotFound verifies 404 for unknown group.
func TestHandleV1SlotsGroup_NotFound(t *testing.T) {
	reg := newTestRegistry()
	dcfg := &config.Config{
		Servers:    map[string]*config.ServerConfig{},
		SlotGroups: map[string]*config.SlotGroupConfig{},
	}
	s := &Server{registry: reg, daemonConfig: dcfg, running: true}

	req := httptest.NewRequest(http.MethodGet, "/v1/slots/nonexistent", nil)
	req.SetPathValue("group", "nonexistent")
	rr := httptest.NewRecorder()
	s.handleV1SlotsGroup(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("unknown group should 404, got %d; body=%s", rr.Code, rr.Body.String())
	}
}

// TestHandleV1SlotsGroup_NotRunningReturns503 mirrors v1_servers behavior.
func TestHandleV1SlotsGroup_NotRunningReturns503(t *testing.T) {
	s := &Server{running: false}
	req := httptest.NewRequest(http.MethodGet, "/v1/slots/playwright", nil)
	req.SetPathValue("group", "playwright")
	rr := httptest.NewRecorder()
	s.handleV1SlotsGroup(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rr.Code)
	}
}

// --- endpoint wiring test ---

// TestHandleV1Slots_EndpointWired confirms the mux registers both /v1/slots
// and /v1/slots/{group} when registerV1Routes is called.
func TestHandleV1Slots_EndpointWired(t *testing.T) {
	reg := newTestRegistry()
	dcfg := &config.Config{
		Servers: map[string]*config.ServerConfig{
			"pw-slot-1": {Port: 7001, Command: "echo", SlotGroup: "playwright", SlotIndex: 1, MaxSessions: 5},
		},
		SlotGroups: map[string]*config.SlotGroupConfig{
			"playwright": {Template: "pw-slot", BasePort: 7001, Count: 1, GroupPort: 7000},
		},
	}
	s := &Server{registry: reg, daemonConfig: dcfg, running: true}

	mux := http.NewServeMux()
	s.registerV1Routes(mux)

	ts := httptest.NewServer(mux)
	defer ts.Close()

	// GET /v1/slots
	resp, err := http.Get(ts.URL + "/v1/slots")
	if err != nil {
		t.Fatalf("GET /v1/slots: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET /v1/slots: status = %d, want 200; body=%s", resp.StatusCode, b)
	}

	// GET /v1/slots/playwright
	resp2, err := http.Get(ts.URL + "/v1/slots/playwright")
	if err != nil {
		t.Fatalf("GET /v1/slots/playwright: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp2.Body)
		t.Fatalf("GET /v1/slots/playwright: status = %d, want 200; body=%s", resp2.StatusCode, b)
	}

	// GET /v1/slots/missing → 404
	resp3, err := http.Get(ts.URL + "/v1/slots/missing")
	if err != nil {
		t.Fatalf("GET /v1/slots/missing: %v", err)
	}
	defer resp3.Body.Close()
	if resp3.StatusCode != http.StatusNotFound {
		t.Errorf("GET /v1/slots/missing: status = %d, want 404", resp3.StatusCode)
	}
}

// --- version capability test ---

// TestVersion_IncludesSlotCapabilities verifies that /version reports
// v1_slots and v1_slot_groups as true.
func TestVersion_IncludesSlotCapabilities(t *testing.T) {
	s := &Server{running: true}
	req := httptest.NewRequest(http.MethodGet, "/version", nil)
	rr := httptest.NewRecorder()
	s.handleVersion(rr, req)

	var body struct {
		API map[string]bool `json:"api"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !body.API["v1_slots"] {
		t.Error("api.v1_slots = false, want true")
	}
	if !body.API["v1_slot_groups"] {
		t.Error("api.v1_slot_groups = false, want true")
	}
}
