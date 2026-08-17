package admin

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/config"
	visionmcp "github.com/Sharper-Flow/Vision-MCP-Manager/internal/mcp"
	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/reachability"
	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/server"
)

// TestReachabilityReporting_AllTransportKinds exercises the same operator
// surfaces for every transport that can be configured. The listener is a real
// loopback HTTP server, so the reachable case proves a client can connect and
// the unreachable case closes that endpoint before collecting failures.
//
// HTTP and SSE are both included because they share the externally-owned
// listener path but have distinct config transport values.
func TestReachabilityReporting_AllTransportKinds(t *testing.T) {
	tests := []struct {
		name      string
		transport config.TransportType
	}{
		{name: "stdio", transport: config.TransportStdio},
		{name: "managed-http", transport: config.TransportManagedHTTP},
		{name: "http", transport: config.TransportHTTP},
		{name: "sse", transport: config.TransportSSE},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			endpoint := httptest.NewServer(
				visionmcp.ListenerProbeMiddleware()(http.NotFoundHandler()),
			)
			defer endpoint.Close()

			probeTransport := &http.Transport{DisableKeepAlives: true}
			probe := &visionmcp.ListenerProbe{Client: &http.Client{Transport: probeTransport}}
			defer probeTransport.CloseIdleConnections()

			port := endpoint.Listener.Addr().(*net.TCPAddr).Port
			name := "reachability-" + strings.ReplaceAll(tc.name, "-", "_")
			reg := newTestRegistry()
			defer reg.Close()
			cfg := &config.ServerConfig{
				Port:      port,
				Transport: tc.transport,
				Command:   "fixture",
				URL:       endpoint.URL + "/mcp",
			}
			if err := reg.Add(name, cfg); err != nil {
				t.Fatal(err)
			}
			srv := reg.Get(name)
			srv.State = server.StateRunning
			srv.StartedAt = time.Now()

			store := reachability.NewStore()
			adminServer := &Server{
				registry:          reg,
				running:           true,
				reachabilityStore: store,
				reachabilityGrace: time.Minute,
			}

			reachable, err := probe.Probe(context.Background(), reachability.Target{Port: port})
			if err != nil || !reachable {
				t.Fatalf("reachable %s endpoint probe = %t, %v", tc.name, reachable, err)
			}
			store.RecordProbe(name, reachability.ProbeResult{
				Depth:   reachability.DepthListener,
				Success: true,
			})
			assertTransportHealthy(t, adminServer, name, tc.transport)

			endpoint.Close()
			for range reachability.FailureThreshold {
				reachable, err = probe.Probe(context.Background(), reachability.Target{Port: port})
				if reachable || err == nil {
					t.Fatalf("closed %s endpoint probe = %t, %v; want failure", tc.name, reachable, err)
				}
				store.RecordProbe(name, reachability.ProbeResult{
					Depth:   reachability.DepthListener,
					Error:   err.Error(),
					Success: false,
				})
			}
			assertTransportUnhealthy(t, adminServer, name, tc.transport)
		})
	}
}

func assertTransportHealthy(t *testing.T, adminServer *Server, name string, transport config.TransportType) {
	t.Helper()
	entries := callV1Servers(t, adminServer)
	if len(entries) != 1 {
		t.Fatalf("%s /v1/servers entries = %d, want 1", transport, len(entries))
	}
	entry := entries[0]
	if entry["transport"] != string(transport) {
		t.Fatalf("transport = %v, want %s", entry["transport"], transport)
	}
	if entry["effective_status"] != "running" {
		t.Fatalf("reachable %s effective_status = %v, want running; entry=%v", transport, entry["effective_status"], entry)
	}
	if entry["reachability"] != string(reachability.StateReachable) || entry["probe_depth"] != string(reachability.DepthListener) || entry["last_probe_outcome"] != string(reachability.OutcomeSuccess) {
		t.Fatalf("reachable %s evidence = %#v, want reachable/listener/success", transport, entry)
	}

	health := callHealth(t, adminServer)
	if health["status"] != "ok" {
		t.Fatalf("reachable %s /health = %v, want ok", transport, health)
	}
}

func assertTransportUnhealthy(t *testing.T, adminServer *Server, name string, transport config.TransportType) {
	t.Helper()
	entries := callV1Servers(t, adminServer)
	entry := entries[0]
	if entry["effective_status"] == "running" {
		t.Fatalf("unreachable %s still reports running: %v", transport, entry)
	}
	if entry["reachability"] != string(reachability.StateUnreachable) || entry["probe_depth"] != string(reachability.DepthListener) || entry["last_probe_outcome"] != string(reachability.OutcomeFailure) {
		t.Fatalf("unreachable %s evidence = %#v, want unreachable/listener/failure", transport, entry)
	}
	if reason, ok := entry["last_probe_error"].(string); !ok || reason == "" {
		t.Fatalf("unreachable %s missing probe reason: %#v", transport, entry)
	}
	if reason, ok := entry["effective_reason"].(string); !ok || reason == "" {
		t.Fatalf("unreachable %s missing effective status reason: %#v", transport, entry)
	}
	if failures, ok := entry["consecutive_probe_failures"].(float64); !ok || int(failures) < reachability.FailureThreshold {
		t.Fatalf("unreachable %s failure streak = %#v, want at least %d", transport, entry["consecutive_probe_failures"], reachability.FailureThreshold)
	}

	health := callHealth(t, adminServer)
	if health["status"] != "degraded" {
		t.Fatalf("unreachable %s /health = %v, want degraded", transport, health)
	}
	errors, ok := health["errors"].([]interface{})
	if !ok || len(errors) != 1 || !strings.Contains(errors[0].(string), name) || errors[0].(string) == "" {
		t.Fatalf("unreachable %s /health errors = %#v, want reason for %s", transport, health["errors"], name)
	}
}

func callV1Servers(t *testing.T, adminServer *Server) []map[string]interface{} {
	t.Helper()
	recorder := httptest.NewRecorder()
	adminServer.handleV1Servers(recorder, httptest.NewRequest(http.MethodGet, "/v1/servers", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("/v1/servers status = %d; body=%s", recorder.Code, recorder.Body.String())
	}
	var body struct {
		Servers []map[string]interface{} `json:"servers"`
	}
	if err := json.NewDecoder(recorder.Body).Decode(&body); err != nil {
		t.Fatalf("decode /v1/servers: %v; body=%s", err, recorder.Body.String())
	}
	return body.Servers
}
