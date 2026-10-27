package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/config"
	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/metrics"
)

func TestSharedTelemetryStatusAndMetrics(t *testing.T) {
	reg := newTestRegistry()
	defer reg.Close()
	if err := reg.Add("shared", &config.ServerConfig{Command: "node", Port: 12345}); err != nil {
		t.Fatal(err)
	}
	var snapshot metrics.ServerMetricsSnapshot
	if err := json.Unmarshal([]byte(`{"spawned_by_reason":{"first_use":1,"idle_reap_respawn":2,"health_respawn":3,"lookup_respawn":4},"shared_downstream":{"pid":123,"started_at":"2026-10-01T00:00:00Z"}}`), &snapshot); err != nil {
		t.Fatal(err)
	}
	s := &Server{registry: reg, running: true, serverMetricsAccessor: &testMetricsAccessor{snapshots: map[string]*metrics.ServerMetricsSnapshot{"shared": &snapshot}}}
	rr := httptest.NewRecorder()
	s.handleV1Servers(rr, httptest.NewRequest(http.MethodGet, "/v1/servers", nil))
	var response struct {
		Servers []map[string]any `json:"servers"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	entry := response.Servers[0]
	if entry["pid"] != float64(123) || entry["started_at"] != "2026-10-01T00:00:00Z" || entry["uptime_seconds"] == nil {
		t.Errorf("shared process identity missing from status: %s", rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"lookup_respawn":4`) {
		t.Error("status omits per-reason spawn counters")
	}
	result, err := s.toolMetrics(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	text := result.Content[0].Text
	if !strings.Contains(text, `"shared_downstream":{"pid":123`) || !strings.Contains(text, `"lookup_respawn":4`) {
		t.Errorf("vision_metrics omits shared identity/counters: %s", text)
	}
	for _, secret := range []string{"args", "env", "command", "token"} {
		if strings.Contains(text, `"`+secret+`"`) {
			t.Errorf("metrics exposes %s", secret)
		}
	}
}
