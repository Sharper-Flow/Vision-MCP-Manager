package mcp

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/config"
	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/metrics"
	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/session"
)

func TestSharedStartingRefusalIsBeforeSDKDispatch(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Fatal(err)
	}
	control, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	ready := make(chan net.Conn, 1)
	go func() {
		c, err := control.Accept()
		if err != nil {
			return
		}
		scanner := bufio.NewScanner(c)
		if scanner.Scan() {
			ready <- c
		}
	}()
	cfg := &config.ServerConfig{Command: "node", Args: []string{"-e", `
const net=require('net'),readline=require('readline');let init;
const c=net.connect({host:'127.0.0.1',port:Number(process.env.CONTROL_PORT)});
const reply=(id,result)=>process.stdout.write(JSON.stringify({jsonrpc:'2.0',id,result})+'\n');
readline.createInterface({input:c}).on('line',()=>reply(init,{protocolVersion:'2025-03-26',capabilities:{tools:{}},serverInfo:{name:'starting',version:'1'}}));
readline.createInterface({input:process.stdin}).on('close',()=>process.exit(0)).on('line',line=>{const m=JSON.parse(line);if(m.method==='initialize'){init=m.id;c.write('initialize\n')};if(m.method==='tools/list')reply(m.id,{tools:[]})});
`}, Env: map[string]string{"CONTROL_PORT": fmt.Sprint(control.Addr().(*net.TCPAddr).Port)}}
	owner := metrics.NewServerMetrics()
	sm := session.NewSharedSessionManager("starting", cfg, testLogger(t), 0, owner)
	defer sm.CloseAll()
	handler := NewProxyHandler(ProxyConfig{ServerName: "starting", SharedManager: sm, Logger: testLogger(t), Metrics: owner})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	const initialize = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(initialize)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { handler.ServeHTTP(w, req); close(done) }()
	var conn net.Conn
	select {
	case conn = <-ready:
		defer conn.Close()
	case <-time.After(3 * time.Second):
		t.Fatal("startup handshake missing")
	}
	// A different, still-connected caller must receive the retryable refusal
	// while the first startup is gated, not wait until its connection cancels.
	waitCtx, cancelWait := context.WithCancel(context.Background())
	waitReq := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(initialize)).WithContext(waitCtx)
	waitReq.Header = req.Header.Clone()
	waitResponse := httptest.NewRecorder()
	waitDone := make(chan struct{})
	go func() { handler.ServeHTTP(waitResponse, waitReq); close(waitDone) }()
	select {
	case <-waitDone:
	case <-time.After(time.Second):
		t.Error("second caller waited for startup instead of receiving starting refusal")
		cancelWait()
		<-waitDone
	}
	cancelWait()
	if waitResponse.Code != http.StatusServiceUnavailable || !strings.Contains(waitResponse.Body.String(), `"type":"starting"`) {
		t.Errorf("waiting caller refusal = %d %s", waitResponse.Code, waitResponse.Body.String())
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("cancelled waiter blocked on shared startup")
	}
	if w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") != "1" || !strings.Contains(w.Body.String(), `"type":"starting"`) || !strings.Contains(w.Body.String(), `"retryable":true`) {
		t.Errorf("startup refusal = %d %s", w.Code, w.Body.String())
	}
	if sm.RefCount() != 0 {
		t.Error("refused initialize reached SDK session admission")
	}
	_, _ = fmt.Fprintln(conn, "release")
	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	if _, err := sm.GetOrCreateSession(ctx2, "warmup"); err != nil {
		t.Fatal(err)
	}
	if err := sm.RemoveSession("warmup"); err != nil {
		t.Fatal(err)
	}
	req2 := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(initialize)).WithContext(ctx2)
	req2.Header = req.Header.Clone()
	w2 := httptest.NewRecorder()
	handler.ServeHTTP(w2, req2)
	if w2.Code != http.StatusOK {
		t.Errorf("later initialize failed: %d %s", w2.Code, w2.Body.String())
	}
	if owner.Snapshot().SpawnedByReason[metrics.SpawnReasonFirstUse] != 1 {
		t.Error("cancelled initialize destroyed shared startup")
	}
}
