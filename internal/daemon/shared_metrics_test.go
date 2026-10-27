package daemon

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/config"
	visionmcp "github.com/Sharper-Flow/Vision-MCP-Manager/internal/mcp"
	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/metrics"
	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/session"
)

func TestSharedTelemetryUsesLifecycleAndMetricsOwners(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	owner := metrics.NewServerMetrics()
	pm := visionmcp.NewPortManager(logger)
	defer pm.Close()
	d := &Daemon{portManager: pm, serverMetrics: map[string]*metrics.ServerMetrics{"shared": owner}}
	cfg := &config.ServerConfig{Command: "node", Args: []string{"-e", `
require('readline').createInterface({input:process.stdin}).on('line',line=>{
const m=JSON.parse(line);
if(m.method==='initialize')process.stdout.write(JSON.stringify({jsonrpc:'2.0',id:m.id,result:{protocolVersion:'2025-03-26',capabilities:{tools:{}},serverInfo:{name:'telemetry',version:'1'}}})+'\n');
});`}, Env: map[string]string{"SECRET_TOKEN": "never-expose-this-credential"}}
	sm := session.NewSharedSessionManager("shared", cfg, logger, 0, owner)
	defer sm.CloseAll()
	if err := pm.AddStreamable("shared", 0, http.NotFoundHandler(), sm); err != nil {
		t.Fatal(err)
	}
	snap := d.ServerMetricsSnapshot("shared")
	if snap.SharedDownstream == nil || snap.SharedDownstream.PID != 0 || snap.SharedDownstream.StartedAt != nil {
		t.Fatalf("unstarted shared process identity = %+v", snap)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := sm.GetOrCreateSession(ctx, "one"); err != nil {
		t.Fatal(err)
	}
	snap = d.ServerMetricsSnapshot("shared")
	pid, started := sm.ProcessStatus()
	if snap.SharedDownstream.PID != pid || pid <= 0 || snap.SharedDownstream.StartedAt == nil || !snap.SharedDownstream.StartedAt.Equal(started) {
		t.Fatalf("snapshot did not use shared owner: %+v", snap)
	}
	if snap.SpawnedByReason[metrics.SpawnReasonFirstUse] != 1 {
		t.Fatalf("snapshot lost spawn counter: %+v", snap)
	}
	encoded, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "SECRET_TOKEN") || strings.Contains(string(encoded), "never-expose") {
		t.Fatalf("telemetry leaked credentials: %s", encoded)
	}
	sm.CloseAll()
	snap = d.ServerMetricsSnapshot("shared")
	if snap.SharedDownstream.PID != 0 || snap.SharedDownstream.StartedAt != nil || snap.SpawnedByReason[metrics.SpawnReasonFirstUse] != 1 {
		t.Fatalf("shutdown must clear identity but retain counters: %+v", snap)
	}
}
