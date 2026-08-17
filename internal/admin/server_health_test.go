package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/config"
	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/reachability"
	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/server"
)

func TestHandleHealthUsesEffectiveReachabilityStatus(t *testing.T) {
	reg := newTestRegistry()
	if err := reg.Add("unreachable", &config.ServerConfig{Transport: config.TransportStdio}); err != nil {
		t.Fatal(err)
	}
	reg.Get("unreachable").State = server.StateRunning
	reg.Get("unreachable").StartedAt = time.Now()

	store := reachability.NewStore()
	for range reachability.FailureThreshold {
		store.RecordProbe("unreachable", reachability.ProbeResult{
			Depth:       reachability.DepthListener,
			Success:     false,
			Error:       "connection refused",
			AttemptedAt: time.Now(),
		})
	}

	s := &Server{
		registry:          reg,
		running:           true,
		reachabilityStore: store,
		reachabilityGrace: time.Minute,
	}

	body := callHealth(t, s)
	if body["status"] != "degraded" {
		t.Fatalf("status=%v, want degraded; body=%v", body["status"], body)
	}
	if _, ok := body["errors"]; !ok {
		t.Fatalf("errors missing for unreachable server: %v", body)
	}
}

func TestHandleHealthEffectiveStatusCasesAndResponseShape(t *testing.T) {
	tests := []struct {
		name         string
		state        server.State
		lastError    string
		reachability reachability.Reachability
		wantStatus   string
		wantKeys     []string
	}{
		{name: "healthy fleet", state: server.StateRunning, reachability: reachableHealthTestValue(), wantStatus: "ok", wantKeys: []string{"status", "uptime"}},
		{name: "failed server", state: server.StateFailed, lastError: "start failed", wantStatus: "degraded", wantKeys: []string{"status", "errors", "uptime"}},
		{name: "crashed server", state: server.StateCrashed, lastError: "process exited", wantStatus: "degraded", wantKeys: []string{"status", "errors", "uptime"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			reg := newTestRegistry()
			if err := reg.Add("server", &config.ServerConfig{Transport: config.TransportStdio}); err != nil {
				t.Fatal(err)
			}
			srv := reg.Get("server")
			srv.State = tc.state
			srv.StartedAt = time.Now()
			if tc.lastError != "" {
				srv.LastError = errors.New(tc.lastError)
			}

			store := reachability.NewStore()
			if tc.reachability.State == reachability.StateReachable {
				store.RecordProbe("server", reachability.ProbeResult{
					Depth:       reachability.DepthListener,
					Success:     true,
					AttemptedAt: time.Now(),
				})
			}
			s := &Server{registry: reg, running: true, reachabilityStore: store, reachabilityGrace: time.Minute}

			body := callHealth(t, s)
			if body["status"] != tc.wantStatus {
				t.Fatalf("status=%v, want %q; body=%v", body["status"], tc.wantStatus, body)
			}
			assertJSONKeys(t, body, tc.wantKeys)
		})
	}
}

func TestHandleHealthUnprobedServerWithinGraceIsNotHealthy(t *testing.T) {
	reg := newTestRegistry()
	if err := reg.Add("warming", &config.ServerConfig{Transport: config.TransportStdio}); err != nil {
		t.Fatal(err)
	}
	srv := reg.Get("warming")
	srv.State = server.StateRunning
	srv.StartedAt = time.Now()

	s := &Server{registry: reg, running: true, reachabilityStore: reachability.NewStore(), reachabilityGrace: time.Minute}
	body := callHealth(t, s)
	if body["status"] != "degraded" {
		t.Fatalf("status=%v, want degraded during grace; body=%v", body["status"], body)
	}
	assertJSONKeys(t, body, []string{"status", "errors", "uptime"})
}

func TestToolStatusDoesNotClaimHealthyWithoutProbeEvidence(t *testing.T) {
	reg := newTestRegistry()
	if err := reg.Add("warming", &config.ServerConfig{Transport: config.TransportStdio}); err != nil {
		t.Fatal(err)
	}
	srv := reg.Get("warming")
	srv.State = server.StateRunning
	srv.StartedAt = time.Now()

	s := &Server{registry: reg, reachabilityStore: reachability.NewStore(), reachabilityGrace: time.Minute}
	result, err := s.toolStatus(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var response StatusResponse
	decodeToolJSON(t, result, &response)
	if response.Healthy {
		t.Fatalf("vision_status reported healthy without probe evidence: %#v", response)
	}
}

func TestHandleHealthzRemainsLivenessOnly(t *testing.T) {
	s := &Server{running: true}
	rr := httptest.NewRecorder()
	s.handleHealthz(rr, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200", rr.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["status"] != "ok" {
		t.Fatalf("body=%v, want status ok", body)
	}
	assertJSONKeys(t, body, []string{"status"})
}

func callHealth(t *testing.T, s *Server) map[string]any {
	t.Helper()
	rr := httptest.NewRecorder()
	s.handleHealth(rr, httptest.NewRequest(http.MethodGet, "/health", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal %q: %v", rr.Body.String(), err)
	}
	return body
}

func assertJSONKeys(t *testing.T, body map[string]any, want []string) {
	t.Helper()
	if len(body) != len(want) {
		t.Fatalf("keys=%v, want exactly %v", body, want)
	}
	for _, key := range want {
		if _, ok := body[key]; !ok {
			t.Fatalf("key %q missing from %v", key, body)
		}
	}
}

func reachableHealthTestValue() reachability.Reachability {
	return reachability.Reachability{
		State: reachability.StateReachable,
		Evidence: map[reachability.Depth]reachability.ProbeEvidence{
			reachability.DepthListener: {
				Depth:            reachability.DepthListener,
				LastProbeAttempt: time.Now(),
				LastProbeOutcome: reachability.OutcomeSuccess,
			},
		},
	}
}
