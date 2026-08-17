package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/config"
	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/reachability"
	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/server"
)

type lifecycleByServer map[string]*SessionLifecycleSnapshot

func (a lifecycleByServer) SessionLifecycleSnapshot(name string) *SessionLifecycleSnapshot {
	return a[name]
}

func TestDeriveEffectiveStatusMatrix(t *testing.T) {
	for _, tc := range []struct {
		name       string
		process    server.State
		backend    string
		reach      reachability.Reachability
		uptime     time.Duration
		grace      time.Duration
		want       string
		wantReason bool
	}{
		{name: "reachable", process: server.StateRunning, backend: "ready", reach: reachableTestValue(), want: "running"},
		{name: "backend starting", process: server.StateRunning, backend: "starting", want: "starting", wantReason: true},
		{name: "probing", process: server.StateRunning, backend: "ready", reach: reachability.Reachability{State: reachability.StateProbing}, want: "starting", wantReason: true},
		{name: "unprobed within grace", process: server.StateRunning, backend: "ready", uptime: 2 * time.Second, grace: 5 * time.Second, want: "starting", wantReason: true},
		{name: "unprobed beyond grace", process: server.StateRunning, backend: "ready", uptime: 6 * time.Second, grace: 5 * time.Second, want: "error", wantReason: true},
		{name: "unreachable threshold", process: server.StateRunning, backend: "ready", reach: unreachableTestValue(), want: "error", wantReason: true},
		{name: "draining", process: server.StateRunning, backend: "draining", reach: reachableTestValue(), want: "error", wantReason: true},
		{name: "recycling", process: server.StateRunning, backend: "recycling", reach: reachableTestValue(), want: "error", wantReason: true},
		{name: "restarting", process: server.StateRunning, backend: "restarting", reach: reachableTestValue(), want: "error", wantReason: true},
		{name: "process failed", process: server.StateFailed, backend: "ready", want: "error", wantReason: true},
		{name: "process crashed", process: server.StateCrashed, backend: "ready", want: "error", wantReason: true},
		{name: "stopped", process: server.StateStopped, backend: "ready", want: "stopped", wantReason: true},
		{name: "stopping", process: server.StateStopping, backend: "ready", want: "stopped", wantReason: true},
		{name: "unknown is never optimistic", process: server.State("mystery"), backend: "ready", want: "error", wantReason: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := deriveEffectiveStatus(tc.process, tc.backend, tc.reach, tc.uptime, tc.grace, "restart authorization=secret")
			if got.Status != tc.want {
				t.Fatalf("status=%q, want %q", got.Status, tc.want)
			}
			if (got.Reason != "") != tc.wantReason {
				t.Fatalf("reason=%q, wantReason=%v", got.Reason, tc.wantReason)
			}
			if strings.Contains(got.Reason, "secret") {
				t.Fatalf("effective reason leaked secret: %q", got.Reason)
			}
			if strings.Contains(got.Reachability.LastProbeError, "secret") {
				t.Fatalf("reachability error leaked secret: %q", got.Reachability.LastProbeError)
			}
		})
	}
}

func TestDeriveEffectiveStatusUsesScrubbedTerminalReason(t *testing.T) {
	got := deriveEffectiveStatus(server.StateFailed, "ready", reachability.Reachability{State: reachability.StateReachable}, 0, 0, "restart limit exceeded authorization=secret")
	if got.Status != "error" || !strings.Contains(got.Reason, "restart limit exceeded") {
		t.Fatalf("effective=%#v", got)
	}
	if strings.Contains(got.Reason, "secret") {
		t.Fatalf("terminal reason leaked secret: %q", got.Reason)
	}
}

func reachableTestValue() reachability.Reachability {
	attempt := time.Unix(1700000000, 0).UTC()
	return reachability.Reachability{
		State: reachability.StateReachable,
		Evidence: map[reachability.Depth]reachability.ProbeEvidence{
			reachability.DepthListener: {
				Depth:            reachability.DepthListener,
				LastProbeAttempt: attempt,
				LastProbeOutcome: reachability.OutcomeSuccess,
			},
		},
	}
}

func unreachableTestValue() reachability.Reachability {
	attempt := time.Unix(1700000000, 0).UTC()
	return reachability.Reachability{
		State: reachability.StateUnreachable,
		Evidence: map[reachability.Depth]reachability.ProbeEvidence{
			reachability.DepthListener: {
				Depth:               reachability.DepthListener,
				LastProbeAttempt:    attempt,
				LastProbeOutcome:    reachability.OutcomeFailure,
				LastProbeError:      "dial failed authorization=secret",
				ConsecutiveFailures: reachability.FailureThreshold,
			},
		},
	}
}

func TestEffectiveStatusParityAcrossListV1AndRestart(t *testing.T) {
	reg := newTestRegistry()
	if err := reg.Add("managed", &config.ServerConfig{Command: "echo", Transport: config.TransportStdio, Port: 16287}); err != nil {
		t.Fatal(err)
	}
	if err := reg.Start("managed"); err != nil {
		t.Fatal(err)
	}
	accessor := lifecycleByServer{"managed": {BackendState: "restarting"}}
	reachabilityStore := reachability.NewStore()
	for range reachability.FailureThreshold {
		reachabilityStore.RecordProbe("managed", reachability.ProbeResult{
			Depth:       reachability.DepthListener,
			Success:     false,
			Error:       "dial failed authorization=secret",
			AttemptedAt: time.Unix(1700000000, 0).UTC(),
		})
	}
	s := &Server{
		registry:                 reg,
		running:                  true,
		sessionLifecycleAccessor: accessor,
		reachabilityStore:        reachabilityStore,
		reachabilityGrace:        time.Minute,
	}

	listResult, err := s.toolList(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var list ListResponse
	decodeToolJSON(t, listResult, &list)
	if len(list.Servers) != 1 || list.Servers[0].Status != "error" || list.Servers[0].ProcessState != "running" {
		t.Fatalf("vision_list=%#v", list.Servers)
	}
	if list.Servers[0].EffectiveReason == nil || *list.Servers[0].EffectiveReason == "" {
		t.Fatal("vision_list missing effective reason")
	}
	if list.Servers[0].Reachability != string(reachability.StateUnreachable) || list.Servers[0].ProbeDepth != string(reachability.DepthListener) || list.Servers[0].LastProbeOutcome != string(reachability.OutcomeFailure) || list.Servers[0].ConsecutiveProbeFailures != reachability.FailureThreshold {
		t.Fatalf("vision_list missing reachability evidence: %#v", list.Servers[0])
	}
	if strings.Contains(list.Servers[0].LastProbeError, "secret") || !strings.Contains(list.Servers[0].LastProbeError, "REDACTED") {
		t.Fatalf("vision_list leaked probe error: %q", list.Servers[0].LastProbeError)
	}

	addResult, err := s.toolAdd(context.Background(), mustJSON(t, map[string]any{"name": "managed"}))
	if err != nil {
		t.Fatal(err)
	}
	var add AddResponse
	decodeToolJSON(t, addResult, &add)
	if add.Status != "error" {
		t.Fatalf("vision_add status=%q, want error", add.Status)
	}
	if add.Reachability != string(reachability.StateUnreachable) || add.LastProbeError == "" || strings.Contains(add.LastProbeError, "secret") {
		t.Fatalf("vision_add missing scrubbed reachability evidence: %#v", add)
	}

	listReq := httptest.NewRequest(http.MethodGet, "/v1/servers", nil)
	listRR := httptest.NewRecorder()
	s.handleV1Servers(listRR, listReq)
	var v1List struct {
		Servers []map[string]any `json:"servers"`
	}
	if err := json.Unmarshal(listRR.Body.Bytes(), &v1List); err != nil {
		t.Fatal(err)
	}
	assertV1EffectiveStatus(t, v1List.Servers[0])
	assertV1Reachability(t, v1List.Servers[0])

	detailReq := httptest.NewRequest(http.MethodGet, "/v1/servers/managed", nil)
	detailReq.SetPathValue("name", "managed")
	detailRR := httptest.NewRecorder()
	s.handleV1ServerDetail(detailRR, detailReq)
	var detail map[string]any
	if err := json.Unmarshal(detailRR.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	assertV1EffectiveStatus(t, detail)
	assertV1Reachability(t, detail)

	restartResult, err := s.toolRestart(context.Background(), mustJSON(t, map[string]any{"name": "managed"}))
	if err != nil {
		t.Fatal(err)
	}
	var restart RestartResponse
	decodeToolJSON(t, restartResult, &restart)
	if restart.Status != "error" || restart.ProcessState != "running" || restart.EffectiveReason == nil {
		t.Fatalf("vision_restart=%#v", restart)
	}
	if restart.Reachability != string(reachability.StateUnreachable) || restart.LastProbeError == "" || strings.Contains(restart.LastProbeError, "secret") {
		t.Fatalf("vision_restart missing scrubbed reachability evidence: %#v", restart)
	}
}

func assertV1EffectiveStatus(t *testing.T, got map[string]any) {
	t.Helper()
	if got["state"] != "running" {
		t.Fatalf("raw V1 state changed: %#v", got)
	}
	if got["process_state"] != "running" || got["effective_status"] != "error" {
		t.Fatalf("effective V1 fields wrong: %#v", got)
	}
	if got["effective_reason"] == "" || got["effective_reason"] == nil {
		t.Fatalf("effective V1 reason missing: %#v", got)
	}
}

func assertV1Reachability(t *testing.T, got map[string]any) {
	t.Helper()
	for _, key := range []string{"reachability", "probe_depth", "last_probe_at", "last_probe_outcome", "last_probe_error", "consecutive_probe_failures"} {
		if _, exists := got[key]; !exists {
			t.Fatalf("missing V1 reachability field %q: %#v", key, got)
		}
	}
	if got["reachability"] != string(reachability.StateUnreachable) || got["probe_depth"] != string(reachability.DepthListener) || got["last_probe_outcome"] != string(reachability.OutcomeFailure) || got["consecutive_probe_failures"] != float64(reachability.FailureThreshold) {
		t.Fatalf("wrong V1 reachability fields: %#v", got)
	}
	lastError, _ := got["last_probe_error"].(string)
	if strings.Contains(lastError, "secret") || !strings.Contains(lastError, "REDACTED") {
		t.Fatalf("V1 probe error leaked: %q", lastError)
	}
}
