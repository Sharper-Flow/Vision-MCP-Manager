package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/config"
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
		want       string
		wantReason bool
	}{
		{name: "ready", process: server.StateRunning, backend: "ready", want: "running"},
		{name: "starting", process: server.StateRunning, backend: "starting", want: "starting"},
		{name: "probing", process: server.StateRunning, backend: "probing", want: "starting"},
		{name: "draining", process: server.StateRunning, backend: "draining", want: "error", wantReason: true},
		{name: "recycling", process: server.StateRunning, backend: "recycling", want: "error", wantReason: true},
		{name: "restarting", process: server.StateRunning, backend: "restarting", want: "error", wantReason: true},
		{name: "process failed", process: server.StateFailed, backend: "ready", want: "error", wantReason: true},
		{name: "process crashed", process: server.StateCrashed, backend: "ready", want: "error", wantReason: true},
		{name: "stopped", process: server.StateStopped, backend: "ready", want: "stopped"},
		{name: "stopping", process: server.StateStopping, backend: "ready", want: "stopped"},
		{name: "unknown is never optimistic", process: server.State("mystery"), backend: "ready", want: "error", wantReason: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := deriveEffectiveStatus(tc.process, tc.backend, "restart token=secret")
			if got.Status != tc.want {
				t.Fatalf("status=%q, want %q", got.Status, tc.want)
			}
			if (got.Reason != "") != tc.wantReason {
				t.Fatalf("reason=%q, wantReason=%v", got.Reason, tc.wantReason)
			}
			if strings.Contains(got.Reason, "secret") {
				t.Fatalf("effective reason leaked secret: %q", got.Reason)
			}
		})
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
	s := &Server{registry: reg, running: true, sessionLifecycleAccessor: accessor}

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

	detailReq := httptest.NewRequest(http.MethodGet, "/v1/servers/managed", nil)
	detailReq.SetPathValue("name", "managed")
	detailRR := httptest.NewRecorder()
	s.handleV1ServerDetail(detailRR, detailReq)
	var detail map[string]any
	if err := json.Unmarshal(detailRR.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	assertV1EffectiveStatus(t, detail)

	restartResult, err := s.toolRestart(context.Background(), mustJSON(t, map[string]any{"name": "managed"}))
	if err != nil {
		t.Fatal(err)
	}
	var restart RestartResponse
	decodeToolJSON(t, restartResult, &restart)
	if restart.Status != "error" || restart.ProcessState != "running" || restart.EffectiveReason == nil {
		t.Fatalf("vision_restart=%#v", restart)
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
