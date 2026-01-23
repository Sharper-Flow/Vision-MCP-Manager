package bridge

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"
)

// DefaultRequestTimeout is the default timeout for requests.
const DefaultRequestTimeout = 60 * time.Second

// DefaultQueueSize is the default maximum number of pending requests.
const DefaultQueueSize = 50

// Errors returned by the bridge.
var (
	ErrBridgeClosed   = errors.New("bridge: closed")
	ErrQueueFull      = errors.New("bridge: request queue full")
	ErrRequestTimeout = errors.New("bridge: request timeout")
	ErrNoResponse     = errors.New("bridge: no response received")
)

// StdioHTTPBridge bridges between stdio (subprocess) and HTTP (clients).
// It handles request correlation, concurrent access, and response routing.
type StdioHTTPBridge struct {
	// I/O to the subprocess
	stdin  io.Writer
	stdout io.Reader

	// Request tracking
	pendingMu sync.Mutex
	pending   map[string]chan *Response

	// ID generation for internal requests
	idGen *IDGenerator

	// Configuration
	requestTimeout time.Duration
	maxQueueSize   int

	// Lifecycle
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	// Logging
	logger *slog.Logger

	// Mutex for writing to stdin (must be serialized)
	writeMu sync.Mutex

	// Closed flag
	closed bool
}

// BridgeOptions configures the bridge.
type BridgeOptions struct {
	RequestTimeout time.Duration
	MaxQueueSize   int
	Logger         *slog.Logger
}

// NewStdioHTTPBridge creates a new bridge for the given stdio pipes.
func NewStdioHTTPBridge(stdin io.Writer, stdout io.Reader, opts *BridgeOptions) *StdioHTTPBridge {
	if opts == nil {
		opts = &BridgeOptions{}
	}
	if opts.RequestTimeout == 0 {
		opts.RequestTimeout = DefaultRequestTimeout
	}
	if opts.MaxQueueSize == 0 {
		opts.MaxQueueSize = DefaultQueueSize
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}

	ctx, cancel := context.WithCancel(context.Background())

	b := &StdioHTTPBridge{
		stdin:          stdin,
		stdout:         stdout,
		pending:        make(map[string]chan *Response),
		idGen:          NewIDGenerator(),
		requestTimeout: opts.RequestTimeout,
		maxQueueSize:   opts.MaxQueueSize,
		ctx:            ctx,
		cancel:         cancel,
		logger:         opts.Logger,
	}

	return b
}

// Start begins reading from stdout and dispatching responses.
func (b *StdioHTTPBridge) Start() {
	b.wg.Add(1)
	go b.readLoop()
}

// Initialize performs the MCP protocol handshake with the server.
// This must be called after Start() and before forwarding any requests.
// Modern MCP servers (especially fastmcp-based ones) require this handshake:
// 1. Client sends "initialize" request
// 2. Server responds with capabilities
// 3. Client sends "notifications/initialized" notification
func (b *StdioHTTPBridge) Initialize(ctx context.Context) error {
	// Build initialize request params
	params := map[string]interface{}{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]interface{}{},
		"clientInfo": map[string]interface{}{
			"name":    "vision",
			"version": "1.0.0",
		},
	}
	paramsJSON, err := json.Marshal(params)
	if err != nil {
		return fmt.Errorf("failed to marshal initialize params: %w", err)
	}

	initReq := &Request{
		JSONRPC: "2.0",
		ID:      b.idGen.Next(),
		Method:  "initialize",
		Params:  paramsJSON,
	}

	// Send initialize request with timeout
	initCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	resp, err := b.SendRequest(initCtx, initReq)
	if err != nil {
		return fmt.Errorf("initialize request failed: %w", err)
	}

	// Check for error response
	if resp.Error != nil {
		return fmt.Errorf("initialize returned error: %s", resp.Error.Message)
	}

	b.logger.Debug("MCP initialize succeeded")

	// Send initialized notification (no ID = notification)
	initializedNotif := &Request{
		JSONRPC: "2.0",
		Method:  "notifications/initialized",
	}

	if err := b.SendNotification(initializedNotif); err != nil {
		return fmt.Errorf("failed to send initialized notification: %w", err)
	}

	b.logger.Debug("MCP session initialized")
	return nil
}

// Close shuts down the bridge.
func (b *StdioHTTPBridge) Close() error {
	b.pendingMu.Lock()
	if b.closed {
		b.pendingMu.Unlock()
		return nil
	}
	b.closed = true
	b.pendingMu.Unlock()

	b.cancel()
	b.wg.Wait()

	// Fail all pending requests
	b.pendingMu.Lock()
	for id, ch := range b.pending {
		close(ch)
		delete(b.pending, id)
	}
	b.pendingMu.Unlock()

	return nil
}

// SendRequest sends a JSON-RPC request and waits for the response.
// The request must have an ID.
func (b *StdioHTTPBridge) SendRequest(ctx context.Context, req *Request) (*Response, error) {
	if req.ID == nil {
		return nil, fmt.Errorf("request must have an ID")
	}

	// Check if bridge is closed
	b.pendingMu.Lock()
	if b.closed {
		b.pendingMu.Unlock()
		return nil, ErrBridgeClosed
	}

	// Check queue size
	if len(b.pending) >= b.maxQueueSize {
		b.pendingMu.Unlock()
		return nil, ErrQueueFull
	}

	// Create response channel
	idStr := ExtractID(req.ID)
	responseCh := make(chan *Response, 1)
	b.pending[idStr] = responseCh
	b.pendingMu.Unlock()

	// Ensure cleanup on exit
	defer func() {
		b.pendingMu.Lock()
		delete(b.pending, idStr)
		b.pendingMu.Unlock()
	}()

	// Send request to stdin
	if err := b.writeMessage(req); err != nil {
		return nil, fmt.Errorf("failed to send request: %w", err)
	}

	// Wait for response with timeout
	timeout := b.requestTimeout
	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining < timeout {
			timeout = remaining
		}
	}

	select {
	case resp, ok := <-responseCh:
		if !ok {
			return nil, ErrBridgeClosed
		}
		return resp, nil
	case <-time.After(timeout):
		return nil, ErrRequestTimeout
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-b.ctx.Done():
		return nil, ErrBridgeClosed
	}
}

// SendNotification sends a JSON-RPC notification (no response expected).
func (b *StdioHTTPBridge) SendNotification(notification *Request) error {
	b.pendingMu.Lock()
	if b.closed {
		b.pendingMu.Unlock()
		return ErrBridgeClosed
	}
	b.pendingMu.Unlock()

	return b.writeMessage(notification)
}

// writeMessage serializes and writes a message to stdin.
func (b *StdioHTTPBridge) writeMessage(msg any) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("failed to marshal message: %w", err)
	}

	b.writeMu.Lock()
	defer b.writeMu.Unlock()

	// Write newline-delimited JSON
	if _, err := b.stdin.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("failed to write to stdin: %w", err)
	}

	return nil
}

// readLoop continuously reads from stdout and dispatches responses.
func (b *StdioHTTPBridge) readLoop() {
	defer b.wg.Done()

	scanner := bufio.NewScanner(b.stdout)
	// Increase buffer size for large responses
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)

	for scanner.Scan() {
		select {
		case <-b.ctx.Done():
			return
		default:
		}

		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		// Parse the message
		msg, err := ParseMessage(line)
		if err != nil {
			b.logger.Debug("failed to parse message from stdout",
				slog.String("error", err.Error()),
				slog.String("line", string(line)),
			)
			continue
		}

		switch m := msg.(type) {
		case *Response:
			b.handleResponse(m)
		case *Request:
			// Server-initiated request (notification or request)
			b.handleServerMessage(m)
		}
	}

	if err := scanner.Err(); err != nil {
		if !errors.Is(err, io.EOF) && !errors.Is(err, context.Canceled) {
			b.logger.Debug("stdout scanner error",
				slog.String("error", err.Error()),
			)
		}
	}
}

// handleResponse routes a response to the waiting request.
func (b *StdioHTTPBridge) handleResponse(resp *Response) {
	idStr := ExtractID(resp.ID)
	if idStr == "" {
		b.logger.Debug("received response without ID")
		return
	}

	b.pendingMu.Lock()
	ch, ok := b.pending[idStr]
	b.pendingMu.Unlock()

	if !ok {
		b.logger.Debug("received response for unknown request",
			slog.String("id", idStr),
		)
		return
	}

	select {
	case ch <- resp:
	default:
		// Channel full, response will be lost
		b.logger.Warn("response channel full, dropping response",
			slog.String("id", idStr),
		)
	}
}

// handleServerMessage handles server-initiated messages.
// This includes notifications and server-to-client requests.
func (b *StdioHTTPBridge) handleServerMessage(req *Request) {
	// For now, just log server notifications
	// In the future, we could fan out to subscribed HTTP clients
	b.logger.Debug("received server message",
		slog.String("method", req.Method),
		slog.Bool("is_notification", req.IsNotification()),
	)
}

// ForwardRequest forwards an HTTP request to the stdio server.
// This is the main entry point for HTTP handlers.
func (b *StdioHTTPBridge) ForwardRequest(ctx context.Context, data []byte) ([]byte, error) {
	// Parse the incoming request
	var req Request
	if err := json.Unmarshal(data, &req); err != nil {
		errResp := NewErrorResponse(nil, NewError(CodeParseError, "Parse error"))
		return json.Marshal(errResp)
	}

	// Validate JSON-RPC version
	if req.JSONRPC != "2.0" {
		errResp := NewErrorResponse(req.ID, NewError(CodeInvalidRequest, "Invalid JSON-RPC version"))
		return json.Marshal(errResp)
	}

	// Handle notifications (no response expected)
	if req.IsNotification() {
		if err := b.SendNotification(&req); err != nil {
			b.logger.Debug("failed to send notification",
				slog.String("method", req.Method),
				slog.String("error", err.Error()),
			)
		}
		return nil, nil // No response for notifications
	}

	// Send request and wait for response
	resp, err := b.SendRequest(ctx, &req)
	if err != nil {
		if errors.Is(err, ErrRequestTimeout) {
			errResp := NewErrorResponse(req.ID, NewError(CodeInternalError, "Request timeout"))
			return json.Marshal(errResp)
		}
		if errors.Is(err, ErrQueueFull) {
			errResp := NewErrorResponse(req.ID, NewError(CodeInternalError, "Server busy"))
			return json.Marshal(errResp)
		}
		errResp := NewErrorResponse(req.ID, NewError(CodeInternalError, err.Error()))
		return json.Marshal(errResp)
	}

	// Return the response
	return json.Marshal(resp)
}

// Stats returns current bridge statistics.
func (b *StdioHTTPBridge) Stats() BridgeStats {
	b.pendingMu.Lock()
	defer b.pendingMu.Unlock()

	return BridgeStats{
		PendingRequests: len(b.pending),
		MaxQueueSize:    b.maxQueueSize,
		RequestTimeout:  b.requestTimeout,
	}
}

// BridgeStats contains bridge statistics.
type BridgeStats struct {
	PendingRequests int           `json:"pending_requests"`
	MaxQueueSize    int           `json:"max_queue_size"`
	RequestTimeout  time.Duration `json:"request_timeout"`
}
