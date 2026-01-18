package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"
)

func TestJSONRPCTypes(t *testing.T) {
	// Test Request creation
	req, err := NewRequest(1, "test", map[string]string{"key": "value"})
	if err != nil {
		t.Fatalf("NewRequest error: %v", err)
	}
	if req.Method != "test" {
		t.Errorf("Method = %q, want %q", req.Method, "test")
	}
	if req.ID != 1 {
		t.Errorf("ID = %v, want 1", req.ID)
	}
	if !req.IsRequest() {
		t.Error("IsRequest() should be true")
	}
	if req.IsNotification() {
		t.Error("IsNotification() should be false")
	}

	// Test notification (nil ID)
	notif, _ := NewRequest(nil, "notify", nil)
	if notif.IsRequest() {
		t.Error("Notification IsRequest() should be false")
	}
	if !notif.IsNotification() {
		t.Error("Notification IsNotification() should be true")
	}

	// Test Response creation
	resp, err := NewResponse(1, map[string]string{"result": "success"})
	if err != nil {
		t.Fatalf("NewResponse error: %v", err)
	}
	if resp.ID != 1 {
		t.Errorf("Response ID = %v, want 1", resp.ID)
	}
	if resp.Error != nil {
		t.Error("Response should not have error")
	}

	// Test Error response
	errResp := NewErrorResponse(1, NewError(CodeInternalError, "test error"))
	if errResp.Error == nil {
		t.Error("Error response should have error")
	}
	if errResp.Error.Code != CodeInternalError {
		t.Errorf("Error code = %d, want %d", errResp.Error.Code, CodeInternalError)
	}
}

func TestParseMessage(t *testing.T) {
	// Parse request
	reqJSON := `{"jsonrpc":"2.0","method":"test","params":{},"id":1}`
	msg, err := ParseMessage([]byte(reqJSON))
	if err != nil {
		t.Fatalf("ParseMessage request error: %v", err)
	}
	req, ok := msg.(*Request)
	if !ok {
		t.Fatalf("Expected *Request, got %T", msg)
	}
	if req.Method != "test" {
		t.Errorf("Method = %q, want %q", req.Method, "test")
	}

	// Parse response
	respJSON := `{"jsonrpc":"2.0","result":{"foo":"bar"},"id":1}`
	msg, err = ParseMessage([]byte(respJSON))
	if err != nil {
		t.Fatalf("ParseMessage response error: %v", err)
	}
	resp, ok := msg.(*Response)
	if !ok {
		t.Fatalf("Expected *Response, got %T", msg)
	}
	if resp.Result == nil {
		t.Error("Response result should not be nil")
	}

	// Parse error response
	errJSON := `{"jsonrpc":"2.0","error":{"code":-32600,"message":"Invalid Request"},"id":1}`
	msg, err = ParseMessage([]byte(errJSON))
	if err != nil {
		t.Fatalf("ParseMessage error response error: %v", err)
	}
	resp, ok = msg.(*Response)
	if !ok {
		t.Fatalf("Expected *Response, got %T", msg)
	}
	if resp.Error == nil {
		t.Error("Error response should have error")
	}
}

func TestExtractID(t *testing.T) {
	tests := []struct {
		id       any
		expected string
	}{
		{nil, ""},
		{"abc", "abc"},
		{float64(123), "123"},
		{42, "42"},
		{int64(999), "999"},
	}

	for _, tt := range tests {
		result := ExtractID(tt.id)
		if result != tt.expected {
			t.Errorf("ExtractID(%v) = %q, want %q", tt.id, result, tt.expected)
		}
	}
}

func TestIDGenerator(t *testing.T) {
	gen := NewIDGenerator()

	id1 := gen.Next()
	id2 := gen.Next()
	id3 := gen.Next()

	if id1 != 1 {
		t.Errorf("First ID = %d, want 1", id1)
	}
	if id2 != 2 {
		t.Errorf("Second ID = %d, want 2", id2)
	}
	if id3 != 3 {
		t.Errorf("Third ID = %d, want 3", id3)
	}
}

// mockPipe creates a connected pair of readers/writers for testing.
type mockPipe struct {
	r *io.PipeReader
	w *io.PipeWriter
}

func newMockPipe() *mockPipe {
	r, w := io.Pipe()
	return &mockPipe{r: r, w: w}
}

func TestStdioHTTPBridge_Basic(t *testing.T) {
	// Create mock pipes
	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	bridge := NewStdioHTTPBridge(stdinW, stdoutR, &BridgeOptions{
		RequestTimeout: 5 * time.Second,
		Logger:         logger,
	})
	bridge.Start()
	defer bridge.Close()

	// Simulate a simple request-response flow
	var wg sync.WaitGroup

	// Mock server: read from stdin, write response to stdout
	wg.Add(1)
	go func() {
		defer wg.Done()
		scanner := make([]byte, 1024)
		n, err := stdinR.Read(scanner)
		if err != nil {
			t.Logf("Mock server read error: %v", err)
			return
		}

		// Parse request
		var req Request
		if err := json.Unmarshal(scanner[:n], &req); err != nil {
			t.Logf("Mock server parse error: %v", err)
			return
		}

		// Send response
		resp := Response{
			JSONRPC: "2.0",
			Result:  json.RawMessage(`{"message":"hello"}`),
			ID:      req.ID,
		}
		respData, _ := json.Marshal(resp)
		stdoutW.Write(append(respData, '\n'))
	}()

	// Send request
	ctx := context.Background()
	req, _ := NewRequest(1, "test", nil)
	resp, err := bridge.SendRequest(ctx, req)

	if err != nil {
		t.Fatalf("SendRequest error: %v", err)
	}
	if resp == nil {
		t.Fatal("Response is nil")
	}
	if resp.Error != nil {
		t.Errorf("Response has error: %v", resp.Error)
	}

	// Cleanup
	stdinW.Close()
	stdoutW.Close()
	wg.Wait()
}

func TestStdioHTTPBridge_Timeout(t *testing.T) {
	// Create mock pipes
	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	bridge := NewStdioHTTPBridge(stdinW, stdoutR, &BridgeOptions{
		RequestTimeout: 100 * time.Millisecond, // Short timeout
		Logger:         logger,
	})
	bridge.Start()
	defer bridge.Close()

	// Don't respond - let it timeout
	go func() {
		// Drain stdin to prevent blocking
		buf := make([]byte, 1024)
		stdinR.Read(buf)
	}()

	ctx := context.Background()
	req, _ := NewRequest(1, "test", nil)
	_, err := bridge.SendRequest(ctx, req)

	if err != ErrRequestTimeout {
		t.Errorf("Expected ErrRequestTimeout, got: %v", err)
	}

	stdinW.Close()
	stdoutW.Close()
}

func TestStdioHTTPBridge_QueueFull(t *testing.T) {
	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	bridge := NewStdioHTTPBridge(stdinW, stdoutR, &BridgeOptions{
		RequestTimeout: 1 * time.Second,
		MaxQueueSize:   2, // Small queue
		Logger:         logger,
	})
	bridge.Start()
	defer bridge.Close()

	// Drain stdin to prevent blocking
	go func() {
		buf := make([]byte, 4096)
		for {
			if _, err := stdinR.Read(buf); err != nil {
				return
			}
		}
	}()

	// Fill the queue (don't respond, so they stay pending)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			ctx := context.Background()
			req, _ := NewRequest(id, "test", nil)
			bridge.SendRequest(ctx, req)
		}(i)
	}

	// Give time for requests to be queued
	time.Sleep(50 * time.Millisecond)

	// Third request should fail with queue full
	ctx := context.Background()
	req, _ := NewRequest(99, "test", nil)
	_, err := bridge.SendRequest(ctx, req)

	if err != ErrQueueFull {
		t.Errorf("Expected ErrQueueFull, got: %v", err)
	}

	stdinW.Close()
	stdoutW.Close()
}

func TestStdioHTTPBridge_ForwardRequest(t *testing.T) {
	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	bridge := NewStdioHTTPBridge(stdinW, stdoutR, &BridgeOptions{
		RequestTimeout: 5 * time.Second,
		Logger:         logger,
	})
	bridge.Start()
	defer bridge.Close()

	// Mock server
	go func() {
		buf := make([]byte, 1024)
		n, _ := stdinR.Read(buf)
		var req Request
		json.Unmarshal(buf[:n], &req)

		resp := Response{
			JSONRPC: "2.0",
			Result:  json.RawMessage(`{"status":"ok"}`),
			ID:      req.ID,
		}
		respData, _ := json.Marshal(resp)
		stdoutW.Write(append(respData, '\n'))
	}()

	// Forward a request
	reqData := []byte(`{"jsonrpc":"2.0","method":"test","params":{},"id":1}`)
	ctx := context.Background()
	respData, err := bridge.ForwardRequest(ctx, reqData)

	if err != nil {
		t.Fatalf("ForwardRequest error: %v", err)
	}

	var resp Response
	if err := json.Unmarshal(respData, &resp); err != nil {
		t.Fatalf("Failed to parse response: %v", err)
	}
	if resp.Error != nil {
		t.Errorf("Response has error: %v", resp.Error)
	}

	stdinW.Close()
	stdoutW.Close()
}

func TestStdioHTTPBridge_Notification(t *testing.T) {
	var stdinBuf bytes.Buffer
	stdoutR, stdoutW := io.Pipe()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	bridge := NewStdioHTTPBridge(&stdinBuf, stdoutR, &BridgeOptions{
		Logger: logger,
	})
	bridge.Start()
	defer bridge.Close()

	// Send notification (no ID)
	notif, _ := NewRequest(nil, "notify", map[string]string{"msg": "hello"})
	err := bridge.SendNotification(notif)
	if err != nil {
		t.Fatalf("SendNotification error: %v", err)
	}

	// Check that it was written to stdin
	written := stdinBuf.String()
	if written == "" {
		t.Error("Notification was not written to stdin")
	}

	var parsed Request
	if err := json.Unmarshal([]byte(written[:len(written)-1]), &parsed); err != nil {
		t.Fatalf("Failed to parse written notification: %v", err)
	}
	if parsed.Method != "notify" {
		t.Errorf("Method = %q, want %q", parsed.Method, "notify")
	}

	stdoutW.Close()
}

func TestStdioHTTPBridge_Stats(t *testing.T) {
	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()

	bridge := NewStdioHTTPBridge(stdinW, stdoutR, &BridgeOptions{
		RequestTimeout: 1 * time.Second,
		MaxQueueSize:   10,
	})
	bridge.Start()
	defer bridge.Close()

	stats := bridge.Stats()
	if stats.PendingRequests != 0 {
		t.Errorf("PendingRequests = %d, want 0", stats.PendingRequests)
	}
	if stats.MaxQueueSize != 10 {
		t.Errorf("MaxQueueSize = %d, want 10", stats.MaxQueueSize)
	}
	if stats.RequestTimeout != 1*time.Second {
		t.Errorf("RequestTimeout = %v, want 1s", stats.RequestTimeout)
	}

	stdinR.Close()
	stdinW.Close()
	stdoutR.Close()
	stdoutW.Close()
}

func TestError_Error(t *testing.T) {
	err := NewError(CodeInternalError, "test error")
	expected := "JSON-RPC error -32603: test error"
	if err.Error() != expected {
		t.Errorf("Error() = %q, want %q", err.Error(), expected)
	}

	errWithData := NewErrorWithData(CodeInvalidParams, "bad params", map[string]string{"field": "name"})
	if errWithData.Data == nil {
		t.Error("Error should have data")
	}
}
