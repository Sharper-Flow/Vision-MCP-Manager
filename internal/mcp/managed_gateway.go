package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/metrics"
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
	IncBackendHeaderTimeout()
	IncReaped(reason string)
}

type ManagedHTTPGatewayConfig struct {
	Target                *url.URL
	MaxSessions           int
	IdleTimeout           time.Duration
	DisconnectGracePeriod time.Duration
	HungRequestBound      time.Duration
	Clock                 LeaseClock
	Transport             http.RoundTripper
	Backend               ManagedBackendGate
	OnAmbiguousFailure    func(error)
	Metrics               ManagedGatewayMetrics
	DaemonMetrics         *metrics.DaemonMetrics
	Logger                *slog.Logger
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
	daemonMetrics      *metrics.DaemonMetrics
	lifecycleMu        sync.Mutex
}

func NewManagedHTTPGateway(cfg ManagedHTTPGatewayConfig) (*ManagedHTTPGateway, error) {
	if cfg.Target == nil {
		return nil, errors.New("managed HTTP target is required")
	}
	if err := validateManagedGatewayTarget(cfg.Target); err != nil {
		return nil, err
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	transport := cfg.Transport
	if transport == nil {
		if base, ok := http.DefaultTransport.(*http.Transport); ok {
			clone := base.Clone()
			clone.ResponseHeaderTimeout = cfg.HungRequestBound
			transport = clone
		} else {
			logger.Warn("http.DefaultTransport is not *http.Transport; using a dedicated transport")
			transport = &http.Transport{ResponseHeaderTimeout: cfg.HungRequestBound}
		}
	}
	target := *cfg.Target
	return &ManagedHTTPGateway{
		target:             &target,
		transport:          transport,
		backend:            cfg.Backend,
		onAmbiguousFailure: cfg.OnAmbiguousFailure,
		leases:             NewLeaseManagerWithDisconnectGrace(cfg.MaxSessions, cfg.IdleTimeout, cfg.DisconnectGracePeriod, cfg.Clock),
		logger:             logger,
		metrics:            cfg.Metrics,
		daemonMetrics:      cfg.DaemonMetrics,
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
				g.countToolsCallsInBody(r)
				writeManagedError(w, http.StatusBadRequest, -32600, "invalid JSON-RPC request")
				return
			}
			if !initialize {
				// Count every JSON-RPC tools/call request even when the
				// session header is missing; the session-header branch owns
				// rejection, not call accounting.
				g.countToolsCallsInBody(r)
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
			activity, toolsCalls, err := classifyManagedRequestBody(r)
			state.toolsCalls = toolsCalls
			if toolsCalls > 0 && g.daemonMetrics != nil {
				for i := 0; i < toolsCalls; i++ {
					g.daemonMetrics.IncToolCalls()
				}
			}
			if err != nil {
				// Genuine members still count when whole-body admission
				// rejects the envelope: the member classifier owns call
				// counting, the admission gate owns rejection.
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
		// A tools/call refused at backend admission is a forwarding failure
		// even though no reverse-proxy callback observed it. Count it once,
		// matching the stdio path's pre-dispatch unavailable failures.
		if state.toolsCalls > 0 && g.daemonMetrics != nil {
			g.daemonMetrics.IncErrors()
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
// successful response or 404. Overdue cleanup-uncertain quarantines are
// released first so capacity pressure is relieved without a retry.
func (g *ManagedHTTPGateway) pruneIdle(ctx context.Context) {
	g.closeExpiredQuarantines()
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
			g.finalizeClose(sessionID, expired.Reason)
			continue
		}
		g.leases.MarkCleanupUncertain(sessionID, "cleanup_rejected")
		g.reportAmbiguousFailure(fmt.Errorf("idle cleanup for %s returned status %d", safeLeaseID(sessionID), resp.StatusCode))
	}
}

// closeExpiredQuarantines releases slots held by cleanup_uncertain leases past
// their bounded quarantine window. The original cleanup attempt stays
// at-most-once; the closed rows keep the quarantine reason.
func (g *ManagedHTTPGateway) closeExpiredQuarantines() {
	g.lifecycleMu.Lock()
	closed := g.leases.ExpireCleanupUncertain()
	g.lifecycleMu.Unlock()
	if g.metrics == nil {
		return
	}
	for range closed {
		g.metrics.DecActiveSessions()
		g.metrics.IncReaped(metrics.ReapReasonCleanupUncertain)
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
			status := http.StatusBadGateway
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				status = http.StatusGatewayTimeout
				if g.metrics != nil {
					g.metrics.IncBackendHeaderTimeout()
				}
			}
			writeManagedError(w, status, -32003, "managed MCP backend request failed")
		},
	}
}

func (g *ManagedHTTPGateway) observeResponse(state *managedProxyRequest, resp *http.Response) error {
	// A non-2xx response to a tools/call is one forwarding failure.
	// A 200 carrying an application-level tool error (isError result) is not.
	if state.toolsCalls > 0 && !isSuccessfulStatus(resp.StatusCode) && g.daemonMetrics != nil {
		g.daemonMetrics.IncErrors()
	}
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
	// A proxy-level dispatch failure on tools/call is one forwarding failure.
	if state.toolsCalls > 0 && g.daemonMetrics != nil {
		g.daemonMetrics.IncErrors()
	}
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
	toolsCalls     int // number of tools/call requests in this JSON-RPC payload
}

// classifyManagedRequestBody reads and restores the JSON-RPC body once and
// reports whether it carries application activity plus how many genuine
// tools/call requests it contains, so daemon-wide tool call counting sees each
// call. Counting is independent of whole-envelope admission: a body whose
// activity classification fails (for example a batch holding one genuine
// request beside an invalid member) still reports its genuine members, and the
// caller counts them before writing the admission rejection.
func classifyManagedRequestBody(r *http.Request) (bool, int, error) {
	if r.Body == nil {
		return false, 0, errors.New("missing JSON-RPC body")
	}
	body, err := readAndRestoreRequestBody(r)
	if err != nil {
		return false, 0, err
	}
	activity, activityErr := ClassifyApplicationActivity(body)
	toolsCalls, _ := countManagedToolsCalls(body)
	return activity, toolsCalls, activityErr
}

// countManagedToolsCalls returns the number of tools/call requests in a
// JSON-RPC envelope or batch. Malformed members count as zero.
func countManagedToolsCalls(body []byte) (int, error) {
	var raw json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return 0, fmt.Errorf("decode JSON-RPC envelope: %w", err)
	}
	if len(raw) == 0 {
		return 0, nil
	}
	if raw[0] == '[' {
		var batch []json.RawMessage
		if err := json.Unmarshal(raw, &batch); err != nil {
			return 0, fmt.Errorf("decode JSON-RPC batch: %w", err)
		}
		count := 0
		for _, member := range batch {
			if isToolsCallMember(member) {
				count++
			}
		}
		return count, nil
	}
	if isToolsCallMember(raw) {
		return 1, nil
	}
	return 0, nil
}

// mcpMemberKind classifies one JSON-RPC envelope member against the MCP
// basic Messages schema (2025-11-25): a request carries jsonrpc "2.0", a
// string-or-integer id, and a method; a notification omits the id; a response
// carries an id with result or error and no method.
type mcpMemberKind uint8

const (
	mcpMemberMalformed mcpMemberKind = iota
	mcpMemberNotification
	mcpMemberRequest
	mcpMemberResponse
)

// mcpMember is one envelope member's classification plus its method name.
type mcpMember struct {
	kind   mcpMemberKind
	method string
}

// classifyMCPMember is the single envelope-classification owner for managed
// gateway counting: request, notification, response, or malformed. It
// validates the envelope only and never reads method params. The go-sdk
// jsonrpc.DecodeMessage is not used here because it accepts a fractional id
// (1.5) as a call by truncating it, while the MCP schema restricts request
// ids to strings and integers.
func classifyMCPMember(raw json.RawMessage) mcpMember {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return mcpMember{kind: mcpMemberMalformed}
	}
	versionRaw, hasVersion := fields["jsonrpc"]
	if !hasVersion {
		return mcpMember{kind: mcpMemberMalformed}
	}
	var version string
	if err := json.Unmarshal(versionRaw, &version); err != nil || version != "2.0" {
		return mcpMember{kind: mcpMemberMalformed}
	}
	idRaw, hasID := fields["id"]
	_, hasResult := fields["result"]
	_, hasError := fields["error"]
	methodRaw, hasMethod := fields["method"]
	if !hasMethod {
		// No method: only a response shape (id with result or error) is
		// recognizable; anything else is malformed.
		if hasID && (hasResult || hasError) {
			return mcpMember{kind: mcpMemberResponse}
		}
		return mcpMember{kind: mcpMemberMalformed}
	}
	var method string
	if err := json.Unmarshal(methodRaw, &method); err != nil || method == "" {
		return mcpMember{kind: mcpMemberMalformed}
	}
	if !hasID {
		return mcpMember{kind: mcpMemberNotification, method: method}
	}
	if hasResult || hasError || !isMCPRequestID(idRaw) {
		return mcpMember{kind: mcpMemberMalformed}
	}
	return mcpMember{kind: mcpMemberRequest, method: method}
}

// isMCPRequestID reports whether the raw JSON value is a valid MCP request
// id: a JSON string, or a JSON number with no fractional part. null,
// booleans, objects, arrays, and fractional numbers are rejected.
func isMCPRequestID(raw json.RawMessage) bool {
	// json.Unmarshal leaves its target untouched with a nil error for a
	// JSON null, so null is rejected before the string probe.
	if string(raw) == "null" {
		return false
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return true
	}
	var number json.Number
	if err := json.Unmarshal(raw, &number); err != nil {
		return false
	}
	literal := number.String()
	if _, err := strconv.ParseInt(literal, 10, 64); err == nil {
		return true
	}
	// JSON Schema "integer" also matches values such as 1.0 or 1e2 whose
	// value carries no fractional part. Decide from the exact literal: a
	// float64 conversion accepts 1.0000000000000001 and
	// 9007199254740992.5 by rounding, and underflows 1e-999 to zero.
	var value big.Rat
	if _, ok := value.SetString(literal); !ok {
		return false
	}
	return value.IsInt()
}

// isToolsCallMember reports whether one JSON-RPC envelope member is a genuine
// MCP tools/call request. The classification owner answers request,
// notification, response, or malformed; only a genuine request whose method
// is tools/call counts.
func isToolsCallMember(raw json.RawMessage) bool {
	member := classifyMCPMember(raw)
	return member.kind == mcpMemberRequest && member.method == "tools/call"
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

// countToolsCallsInBody counts the tools/call requests in this POST body,
// batch members included, for the daemon-wide call counter. Call accounting
// is independent of the session-header branch: a request rejected before any
// session lookup still counted its calls.
func (g *ManagedHTTPGateway) countToolsCallsInBody(r *http.Request) {
	if g.daemonMetrics == nil {
		return
	}
	// Counting is independent of whole-body admission: the classification
	// error marks a body the gate will reject, and genuine members still
	// count through that rejection.
	_, toolsCalls, _ := classifyManagedRequestBody(r)
	if toolsCalls == 0 {
		return
	}
	for i := 0; i < toolsCalls; i++ {
		g.daemonMetrics.IncToolCalls()
	}
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
