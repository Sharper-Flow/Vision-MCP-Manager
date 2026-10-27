package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/config"
	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/metrics"
	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/session"
)

type startupWaitContext struct {
	context.Context
	entered chan struct{}
	once    sync.Once
}

func (c *startupWaitContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.entered) })
	return c.Context.Done()
}

func TestSharedConcurrentColdInitializersWaitAndIsolateCancellation(t *testing.T) {
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
	const parallel = 5
	responses := make([]*httptest.ResponseRecorder, parallel)
	finished := make([]chan struct{}, parallel)
	for i := range parallel {
		waitCtx, cancelWait := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancelWait()
		observed := &startupWaitContext{Context: waitCtx, entered: make(chan struct{})}
		waitReq := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(initialize)).WithContext(observed)
		waitReq.Header = req.Header.Clone()
		responses[i] = httptest.NewRecorder()
		finished[i] = make(chan struct{})
		go func() { handler.ServeHTTP(responses[i], waitReq); close(finished[i]) }()
		select {
		case <-observed.entered:
		case <-finished[i]:
			t.Errorf("parallel initializer %d refused before its wait expired: %d %s", i, responses[i].Code, responses[i].Body.String())
		case <-time.After(3 * time.Second):
			t.Fatal("parallel initializer did not reach startup wait")
		}
	}
	deadlineCtx, cancelDeadline := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancelDeadline()
	deadlineReq := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":19,"method":"initialize","params":{}}`)).WithContext(deadlineCtx)
	deadlineResponse := httptest.NewRecorder()
	handler.ServeHTTP(deadlineResponse, deadlineReq)
	if deadlineCtx.Err() != context.DeadlineExceeded || deadlineResponse.Code != http.StatusServiceUnavailable || !strings.Contains(deadlineResponse.Body.String(), `"reason":"deadline_exceeded"`) || !strings.Contains(deadlineResponse.Body.String(), `"type":"starting"`) || !strings.Contains(deadlineResponse.Body.String(), `"id":19`) {
		t.Errorf("readiness wait refused before its own deadline or lost identity: %d %s", deadlineResponse.Code, deadlineResponse.Body.String())
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("cancelled waiter blocked on shared startup")
	}
	if w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") != "1" || !strings.Contains(w.Body.String(), `"type":"starting"`) || !strings.Contains(w.Body.String(), `"retryable":true`) || !strings.Contains(w.Body.String(), `"reason":"context_canceled"`) || !strings.Contains(w.Body.String(), `"id":1`) {
		t.Errorf("startup refusal = %d %s", w.Code, w.Body.String())
	}
	if sm.RefCount() != 0 {
		t.Error("refused initialize reached SDK session admission")
	}
	_, _ = fmt.Fprintln(conn, "release")
	for i := range parallel {
		select {
		case <-finished[i]:
		case <-time.After(3 * time.Second):
			t.Fatal("initializer did not resume after startup")
		}
		if responses[i].Code != http.StatusOK {
			t.Errorf("parallel initializer %d = %d %s", i, responses[i].Code, responses[i].Body.String())
		}
	}
	if owner.Snapshot().SpawnedByReason[metrics.SpawnReasonFirstUse] != 1 {
		t.Error("cancelled initialize destroyed shared startup")
	}
}

func TestSharedStartupFailureIsStructuredAndSecretSafe(t *testing.T) {
	cfg := &config.ServerConfig{Command: "vision-missing-SECRET_TOKEN=do-not-expose"}
	sm := session.NewSharedSessionManager("failed", cfg, testLogger(t), 0, nil)
	defer sm.CloseAll()
	h := NewProxyHandler(ProxyConfig{ServerName: "failed", SharedManager: sm, Logger: testLogger(t)})
	r := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":17,"method":"initialize","params":{}}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	var response struct {
		ID    int `json:"id"`
		Error struct {
			Data struct {
				Type      string `json:"type"`
				Retryable bool   `json:"retryable"`
			} `json:"data"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatalf("startup failure is not structured JSON: %d %s", w.Code, w.Body.String())
	}
	if w.Code != http.StatusServiceUnavailable || response.ID != 17 || response.Error.Data.Type != "startup_failed" || !response.Error.Data.Retryable || w.Header().Get(gatewayDenialHeader) != "shared-startup_failed" {
		t.Errorf("startup failure identity = %d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "do-not-expose") || strings.Contains(w.Body.String(), "SECRET_TOKEN") {
		t.Errorf("failure leaked credentials: %s", w.Body.String())
	}
}

func TestSharedStartupTimeoutIsStructured(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Fatal(err)
	}
	cfg := &config.ServerConfig{Command: "node", Args: []string{"-e", `require('readline').createInterface({input:process.stdin}).on('line',()=>{}).on('close',()=>process.exit(0));`}}
	sm := session.NewSharedSessionManager("timeout", cfg, testLogger(t), 0, nil)
	defer sm.CloseAll()
	h := NewProxyHandler(ProxyConfig{ServerName: "timeout", SharedManager: sm, Logger: testLogger(t)})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	r := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":18,"method":"initialize","params":{}}`)).WithContext(ctx)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	var response struct {
		ID    int `json:"id"`
		Error struct {
			Data struct {
				Type      string `json:"type"`
				Reason    string `json:"reason"`
				Retryable bool   `json:"retryable"`
			} `json:"data"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if w.Code != http.StatusServiceUnavailable || response.ID != 18 || response.Error.Data.Type != "startup_failed" || response.Error.Data.Reason != "deadline_exceeded" || !response.Error.Data.Retryable {
		t.Errorf("startup timeout = %d %s", w.Code, w.Body.String())
	}
}
