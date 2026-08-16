package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"
)

const managedProbeInitialize = `{"jsonrpc":"2.0","id":"vision-readiness","method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"vision-readiness","version":"1.0.0"}}}`

// ManagedBackendGate blocks dispatch while the supervised native HTTP backend
// is starting, draining, or restarting.
type ManagedBackendGate interface {
	BeginRequest() (func(), error)
	Ready() bool
}

type ManagedGatewayMetrics interface {
	IncActiveSessions()
	DecActiveSessions()
	IncAdmissionDenied()
	IncReaped(reason string)
}

type ManagedHTTPGatewayConfig struct {
	Target             *url.URL
	MaxSessions        int
	IdleTimeout        time.Duration
	Clock              LeaseClock
	Transport          http.RoundTripper
	Backend            ManagedBackendGate
	OnAmbiguousFailure func(error)
	Metrics            ManagedGatewayMetrics
	Logger             *slog.Logger
}

// ManagedHTTPGateway is the lifecycle-aware boundary between Vision's public
// MCP listener and one loopback-only native Streamable HTTP MCP backend.
// It preserves backend-generated session IDs and forwards every request at
// most once.
type ManagedHTTPGateway struct {
	target             *url.URL
	transport          http.RoundTripper
	backend            ManagedBackendGate
	onAmbiguousFailure func(error)
	leases             *LeaseManager
	logger             *slog.Logger
	metrics            ManagedGatewayMetrics
	lifecycleMu        sync.Mutex
}

func NewManagedHTTPGateway(cfg ManagedHTTPGatewayConfig) (*ManagedHTTPGateway, error) {
	if cfg.Target == nil {
		return nil, errors.New("managed HTTP target is required")
	}
	if err := validateManagedGatewayTarget(cfg.Target); err != nil {
		return nil, err
	}
	transport := cfg.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	target := *cfg.Target
	return &ManagedHTTPGateway{
		target:             &target,
		transport:          transport,
		backend:            cfg.Backend,
		onAmbiguousFailure: cfg.OnAmbiguousFailure,
		leases:             NewLeaseManager(cfg.MaxSessions, cfg.IdleTimeout, cfg.Clock),
		logger:             logger,
		metrics:            cfg.Metrics,
	}, nil
}

// ProbeManagedHTTPBackend verifies native MCP readiness with one bounded
// initialize/delete pair. It never retries either request. Callers must recycle
// the backend before another probe when this function returns after initialize
// may have reached the server.
func ProbeManagedHTTPBackend(ctx context.Context, target *url.URL, transport http.RoundTripper) error {
	if target == nil {
		return errors.New("managed HTTP probe target is required")
	}
	if transport == nil {
		transport = http.DefaultTransport
	}

	initialize, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), strings.NewReader(managedProbeInitialize))
	if err != nil {
		return fmt.Errorf("build readiness initialize: %w", err)
	}
	initialize.Header.Set("Content-Type", "application/json")
	initialize.Header.Set("Accept", "application/json, text/event-stream")
	initialize.Header.Set("Mcp-Protocol-Version", "2025-03-26")
	resp, err := transport.RoundTrip(initialize)
	if err != nil {
		return fmt.Errorf("readiness initialize: %w", err)
	}
	_ = resp.Body.Close()
	if !isSuccessfulStatus(resp.StatusCode) {
		return fmt.Errorf("readiness initialize status %d", resp.StatusCode)
	}
	sessionID := strings.TrimSpace(resp.Header.Get("Mcp-Session-Id"))
	if sessionID == "" {
		return errors.New("readiness initialize missing Mcp-Session-Id")
	}

	cleanup, err := http.NewRequestWithContext(ctx, http.MethodDelete, target.String(), nil)
	if err != nil {
		return fmt.Errorf("build readiness cleanup: %w", err)
	}
	cleanup.Header.Set("Mcp-Session-Id", sessionID)
	cleanup.Header.Set("Mcp-Protocol-Version", "2025-03-26")
	cleanupResp, err := transport.RoundTrip(cleanup)
	if err != nil {
		return fmt.Errorf("readiness cleanup: %w", err)
	}
	_ = cleanupResp.Body.Close()
	if !isSuccessfulStatus(cleanupResp.StatusCode) && cleanupResp.StatusCode != http.StatusNotFound {
		return fmt.Errorf("readiness cleanup status %d", cleanupResp.StatusCode)
	}
	return nil
}

func (g *ManagedHTTPGateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/mcp" {
		http.NotFound(w, r)
		return
	}

	sessionID := r.Header.Get("Mcp-Session-Id")
	state := &managedProxyRequest{sessionID: sessionID}

	switch r.Method {
	case http.MethodPost:
		if sessionID == "" {
			initialize, err := isManagedInitializeRequest(r)
			if err != nil {
				writeManagedError(w, http.StatusBadRequest, -32600, "invalid JSON-RPC request")
				return
			}
			if !initialize {
				writeManagedError(w, http.StatusNotFound, -32001, "MCP session not found")
				return
			}
			state.kind = managedRequestInitialize
			reservation, err := g.reserveWithPrune(r.Context())
			if err != nil {
				if g.metrics != nil {
					g.metrics.IncAdmissionDenied()
				}
				writeManagedError(w, http.StatusTooManyRequests, -32000,
					fmt.Sprintf("max sessions reached: limit %d (current %d)", g.leases.Snapshot(0).CapacityMax, g.leases.CapacityUsed()))
				return
			}
			state.reservation = reservation
			state.hasReservation = true
		} else {
			activity, err := classifyManagedRequestBody(r)
			if err != nil {
				writeManagedError(w, http.StatusBadRequest, -32600, "invalid JSON-RPC request")
				return
			}
			complete, err := g.leases.BeginRequest(sessionID, activity)
			if err != nil {
				writeManagedError(w, http.StatusNotFound, -32001, "MCP session not found")
				return
			}
			defer complete()
			state.kind = managedRequestSession
		}

	case http.MethodGet:
		if sessionID == "" || g.leases.BeginSSE(sessionID) != nil {
			writeManagedError(w, http.StatusNotFound, -32001, "MCP session not found")
			return
		}
		defer g.leases.EndSSE(sessionID)
		state.kind = managedRequestSSE

	case http.MethodDelete:
		if sessionID == "" || !g.leases.TryBeginExpiry(sessionID, "client_delete") {
			writeManagedError(w, http.StatusNotFound, -32001, "MCP session not found")
			return
		}
		state.kind = managedRequestDelete

	default:
		w.Header().Set("Allow", "GET, POST, DELETE")
		writeManagedError(w, http.StatusMethodNotAllowed, -32600, "method not allowed")
		return
	}

	var finishBackend func()
	var err error
	if state.kind == managedRequestSSE {
		finishBackend, err = g.beginBackendStream()
	} else {
		finishBackend, err = g.beginBackendRequest()
	}
	if err != nil {
		if state.hasReservation {
			g.leases.ReleaseReservation(state.reservation)
		}
		if state.kind == managedRequestDelete {
			g.leases.RestoreActive(state.sessionID)
		}
		writeManagedError(w, http.StatusServiceUnavailable, -32002, "managed MCP backend unavailable")
		return
	}
	defer finishBackend()

	g.proxy(state).ServeHTTP(w, r)
}

func (g *ManagedHTTPGateway) beginBackendRequest() (func(), error) {
	if g.backend == nil {
		return func() {}, nil
	}
	return g.backend.BeginRequest()
}

func (g *ManagedHTTPGateway) beginBackendStream() (func(), error) {
	if g.backend == nil {
		return func() {}, nil
	}
	if !g.backend.Ready() {
		return nil, errors.New("managed MCP backend unavailable")
	}
	// SSE connectivity is deliberately excluded from application in-flight
	// accounting so stale reconnects cannot prevent expiry or recycle.
	return func() {}, nil
}

func (g *ManagedHTTPGateway) reserveWithPrune(ctx context.Context) (Reservation, error) {
	reservation, err := g.leases.Reserve()
	if !errors.Is(err, ErrLeaseCapacity) {
		return reservation, err
	}
	g.pruneIdle(ctx)
	return g.leases.Reserve()
}

// pruneIdle performs one at-most-once DELETE for each newly expiry-eligible
// lease. Capacity is released only when the backend proves disposal with a
// successful response or 404.
func (g *ManagedHTTPGateway) pruneIdle(ctx context.Context) {
	for _, expired := range g.leases.ExpireEligible() {
		sessionID := expired.SessionID
		req, err := http.NewRequestWithContext(ctx, http.MethodDelete, g.target.String(), nil)
		if err != nil {
			g.leases.MarkCleanupUncertain(sessionID, "cleanup_request_invalid")
			continue
		}
		req.Header.Set("Mcp-Session-Id", sessionID)
		finish, gateErr := g.beginBackendRequest()
		if gateErr != nil {
			g.leases.RestoreActive(sessionID)
			continue
		}
		resp, roundTripErr := g.transport.RoundTrip(req)
		finish()
		if roundTripErr != nil {
			g.leases.MarkCleanupUncertain(sessionID, "cleanup_result_ambiguous")
			g.reportAmbiguousFailure(fmt.Errorf("idle cleanup for %s: %w", safeLeaseID(sessionID), roundTripErr))
			continue
		}
		_ = resp.Body.Close()
		if isSuccessfulStatus(resp.StatusCode) || resp.StatusCode == http.StatusNotFound {
			g.finalizeClose(sessionID, "idle_timeout")
			continue
		}
		g.leases.MarkCleanupUncertain(sessionID, "cleanup_rejected")
		g.reportAmbiguousFailure(fmt.Errorf("idle cleanup for %s returned status %d", safeLeaseID(sessionID), resp.StatusCode))
	}
}

func (g *ManagedHTTPGateway) proxy(state *managedProxyRequest) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = g.target.Scheme
			req.URL.Host = g.target.Host
			req.URL.Path = g.target.Path
			req.URL.RawPath = ""
			req.URL.RawQuery = ""
			req.Host = g.target.Host
			req.Header.Del("Authorization")
			req.Header.Del("Origin")
			req.Header.Del("Forwarded")
			req.Header.Del("X-Forwarded-Host")
			req.Header.Del("X-Forwarded-Proto")
			req.Header["X-Forwarded-For"] = nil
		},
		Transport:     g.transport,
		FlushInterval: -1,
		ModifyResponse: func(resp *http.Response) error {
			return g.observeResponse(state, resp)
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			g.observeProxyError(state, err)
			writeManagedError(w, http.StatusBadGateway, -32003, "managed MCP backend request failed")
		},
	}
}

func (g *ManagedHTTPGateway) observeResponse(state *managedProxyRequest, resp *http.Response) error {
	switch state.kind {
	case managedRequestInitialize:
		if !isSuccessfulStatus(resp.StatusCode) {
			g.leases.ReleaseReservation(state.reservation)
			state.hasReservation = false
			return nil
		}
		sessionID := strings.TrimSpace(resp.Header.Get("Mcp-Session-Id"))
		if sessionID == "" {
			return errors.New("managed MCP initialize response missing Mcp-Session-Id")
		}
		g.lifecycleMu.Lock()
		if err := g.leases.Commit(state.reservation, sessionID); err != nil {
			g.lifecycleMu.Unlock()
			return fmt.Errorf("commit managed MCP session: %w", err)
		}
		if g.metrics != nil {
			g.metrics.IncActiveSessions()
		}
		g.lifecycleMu.Unlock()
		state.hasReservation = false

	case managedRequestDelete:
		if isSuccessfulStatus(resp.StatusCode) || resp.StatusCode == http.StatusNotFound {
			g.finalizeClose(state.sessionID, "upstream_delete")
		} else {
			g.leases.MarkCleanupUncertain(state.sessionID, "cleanup_rejected")
			g.reportAmbiguousFailure(fmt.Errorf("session cleanup returned status %d", resp.StatusCode))
		}
	}
	return nil
}

func (g *ManagedHTTPGateway) observeProxyError(state *managedProxyRequest, err error) {
	switch state.kind {
	case managedRequestInitialize:
		// Once RoundTrip begins, Vision cannot prove whether initialize executed.
		// Keep the reservation quarantined until backend recycle.
		g.logger.Warn("managed MCP initialize result ambiguous", slog.String("error", err.Error()))
	case managedRequestDelete:
		g.leases.MarkCleanupUncertain(state.sessionID, "cleanup_result_ambiguous")
	}
	g.reportAmbiguousFailure(err)
}

func (g *ManagedHTTPGateway) reportAmbiguousFailure(err error) {
	if g.onAmbiguousFailure != nil {
		g.onAmbiguousFailure(err)
	}
}

func (g *ManagedHTTPGateway) Snapshot(limit int) LeaseSnapshot {
	return g.leases.Snapshot(limit)
}

func (g *ManagedHTTPGateway) finalizeClose(sessionID, reason string) {
	g.lifecycleMu.Lock()
	defer g.lifecycleMu.Unlock()
	if !g.leases.FinalizeClose(sessionID) {
		return
	}
	if g.metrics != nil {
		g.metrics.DecActiveSessions()
		g.metrics.IncReaped(reason)
	}
}

func (g *ManagedHTTPGateway) StartReaper(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				g.pruneIdle(ctx)
			}
		}
	}()
}

// CloseAll implements SessionCloser. Listener teardown coincides with backend
// process teardown, so all in-memory claims can be invalidated without sending
// synthetic DELETE requests.
func (g *ManagedHTTPGateway) CloseAll() {
	g.InvalidateAll("listener_closed")
}

func (g *ManagedHTTPGateway) InvalidateAll(reason string) {
	g.lifecycleMu.Lock()
	defer g.lifecycleMu.Unlock()
	closed := g.leases.InvalidateAll(reason)
	if g.metrics == nil {
		return
	}
	for i := 0; i < closed; i++ {
		g.metrics.DecActiveSessions()
		g.metrics.IncReaped(reason)
	}
}

type managedRequestKind uint8

const (
	managedRequestSession managedRequestKind = iota
	managedRequestInitialize
	managedRequestSSE
	managedRequestDelete
)

type managedProxyRequest struct {
	kind           managedRequestKind
	sessionID      string
	reservation    Reservation
	hasReservation bool
}

func classifyManagedRequestBody(r *http.Request) (bool, error) {
	if r.Body == nil {
		return false, errors.New("missing JSON-RPC body")
	}
	body, err := readAndRestoreRequestBody(r)
	if err != nil {
		return false, err
	}
	return ClassifyApplicationActivity(body)
}

func isManagedInitializeRequest(r *http.Request) (bool, error) {
	if r.Body == nil {
		return false, errors.New("missing JSON-RPC body")
	}
	body, err := readAndRestoreRequestBody(r)
	if err != nil {
		return false, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return false, err
	}
	methodRaw, hasMethod := fields["method"]
	_, hasID := fields["id"]
	_, hasResult := fields["result"]
	_, hasError := fields["error"]
	if !hasMethod || !hasID || hasResult || hasError {
		return false, nil
	}
	var method string
	if err := json.Unmarshal(methodRaw, &method); err != nil {
		return false, err
	}
	return method == "initialize", nil
}

func validateManagedGatewayTarget(target *url.URL) error {
	if target.Scheme != "http" || target.Path != "/mcp" || target.Host == "" {
		return errors.New("managed HTTP target must be an http URL with exact /mcp path")
	}
	host := target.Hostname()
	if host != "localhost" {
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			return errors.New("managed HTTP target host must be loopback")
		}
	}
	if target.User != nil || target.RawQuery != "" || target.Fragment != "" {
		return errors.New("managed HTTP target cannot contain userinfo, query, or fragment")
	}
	return nil
}

func readAndRestoreRequestBody(r *http.Request) ([]byte, error) {
	original := r.Body
	body, err := io.ReadAll(original)
	_ = original.Close()
	if err != nil {
		return nil, err
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	return body, nil
}

func isSuccessfulStatus(status int) bool { return status >= 200 && status < 300 }

func writeManagedError(w http.ResponseWriter, status, code int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"jsonrpc": "2.0",
		"error":   map[string]any{"code": code, "message": message},
	})
}
