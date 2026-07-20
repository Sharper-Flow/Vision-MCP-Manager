package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/config"
)

type testLifecycleAccessor struct {
	snapshot *SessionLifecycleSnapshot
}

func (a *testLifecycleAccessor) SessionLifecycleSnapshot(string) *SessionLifecycleSnapshot {
	return a.snapshot
}

func TestServerDiagnosticsExposeBoundedSafeLifecycleSnapshot(t *testing.T) {
	reg := newTestRegistry()
	if err := reg.Add("playwright", &config.ServerConfig{Port: 6287}); err != nil {
		t.Fatal(err)
	}
	accessor := &testLifecycleAccessor{snapshot: &SessionLifecycleSnapshot{
		BackendState: "ready",
		CapacityUsed: 1,
		CapacityMax:  6,
		Sessions: []SessionLifecycleRow{{
			SafeID: "12-char-hash", State: "active", AgeSeconds: 120,
			ApplicationIdleSeconds: 60, InFlight: 1, SSEConnections: 2,
			LifecycleReason: "initialized",
		}},
		Closed: []SessionLifecycleRow{{
			SafeID: "closed-hash", State: "closed", LifecycleReason: "idle_timeout",
		}},
		Omitted: 3,
	}}
	s := &Server{registry: reg, running: true, sessionLifecycleAccessor: accessor}
	req := httptest.NewRequest(http.MethodGet, "/v1/servers/playwright", nil)
	req.SetPathValue("name", "playwright")
	resp := httptest.NewRecorder()
	s.handleV1ServerDetail(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.Code, resp.Body.String())
	}
	if strings.Contains(resp.Body.String(), "raw-private-session-id") {
		t.Fatal("diagnostics exposed raw session ID")
	}
	var body map[string]any
	if err := json.Unmarshal(resp.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	lifecycle, ok := body["session_lifecycle"].(map[string]any)
	if !ok {
		t.Fatalf("session_lifecycle missing: %#v", body)
	}
	if lifecycle["capacity_used"] != float64(1) || lifecycle["capacity_max"] != float64(6) {
		t.Fatalf("capacity fields=%#v", lifecycle)
	}
	rows := lifecycle["sessions"].([]any)
	row := rows[0].(map[string]any)
	for _, key := range []string{"safe_id", "state", "age_seconds", "application_idle_seconds", "in_flight", "sse_connections", "lifecycle_reason"} {
		if _, exists := row[key]; !exists {
			t.Fatalf("missing lifecycle field %q in %#v", key, row)
		}
	}

	toolResult, err := s.toolList(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var list ListResponse
	if err := json.Unmarshal([]byte(toolResult.Content[0].Text), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Servers) != 1 || list.Servers[0].SessionLifecycle == nil {
		t.Fatalf("vision_list missing session lifecycle: %#v", list.Servers)
	}
}
