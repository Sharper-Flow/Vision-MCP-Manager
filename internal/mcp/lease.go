package mcp

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

var (
	ErrLeaseCapacity       = errors.New("mcp lease capacity reached")
	ErrLeaseNotActive      = errors.New("mcp lease is not active")
	ErrReservationNotFound = errors.New("mcp lease reservation not found")
	ErrLeaseExists         = errors.New("mcp lease already exists")
)

// LeaseClock makes lease expiry deterministic in tests.
type LeaseClock interface {
	Now() time.Time
}

type realLeaseClock struct{}

func (realLeaseClock) Now() time.Time { return time.Now() }

type LeaseState string

const (
	LeaseStateActive           LeaseState = "active"
	LeaseStateExpiring         LeaseState = "expiring"
	LeaseStateCleanupUncertain LeaseState = "cleanup_uncertain"
	LeaseStateClosed           LeaseState = "closed"
	maxClosedLeaseHistory                 = 1000
)

// Reservation is an opaque capacity claim created before a downstream server
// returns its session identifier.
type Reservation struct{ token uint64 }

type leaseRecord struct {
	sessionID          string
	safeID             string
	state              LeaseState
	createdAt          time.Time
	lastActivity       time.Time
	inFlight           int
	sseCount           int
	everStreamed       bool
	disconnectDeadline time.Time
	reason             string
}

type LeaseSnapshotRow struct {
	SafeID          string
	State           LeaseState
	Age             time.Duration
	Idle            time.Duration
	InFlight        int
	SSEConnections  int
	LifecycleReason string
}

type LeaseSnapshot struct {
	CapacityUsed  int
	CapacityMax   int
	Rows          []LeaseSnapshotRow
	Omitted       int
	Closed        []LeaseSnapshotRow
	ClosedOmitted int
}

type ExpiredLease struct {
	SessionID string
	Reason    string
}

// LeaseManager is the sole mutation authority for managed HTTP admission and
// per-session lifecycle state.
type LeaseManager struct {
	mu                 sync.Mutex
	maxSessions        int
	idleTimeout        time.Duration
	disconnectGrace    time.Duration
	neverStreamedBound time.Duration
	clock              LeaseClock
	nextToken          atomic.Uint64
	reservations       map[uint64]struct{}
	leases             map[string]*leaseRecord
	closed             []LeaseSnapshotRow
	closedOmitted      int
}

func NewLeaseManager(maxSessions int, idleTimeout time.Duration, clock LeaseClock) *LeaseManager {
	return NewLeaseManagerWithDisconnectGrace(maxSessions, idleTimeout, 0, clock)
}

func NewLeaseManagerWithDisconnectGrace(maxSessions int, idleTimeout, disconnectGrace time.Duration, clock LeaseClock) *LeaseManager {
	if clock == nil {
		clock = realLeaseClock{}
	}
	// A lease that never opened a stream is reclaimed faster than the full idle
	// timeout, but the bound must never resolve to 0: that would make the rule-2
	// comparison trivially true and reap every never-streamed lease on sight.
	// A non-positive bound disables the rule entirely.
	neverStreamedBound := idleTimeout
	if disconnectGrace > 0 {
		graceBound := 5 * disconnectGrace
		if neverStreamedBound <= 0 || graceBound < neverStreamedBound {
			neverStreamedBound = graceBound
		}
	}
	return &LeaseManager{
		maxSessions:        maxSessions,
		idleTimeout:        idleTimeout,
		disconnectGrace:    disconnectGrace,
		neverStreamedBound: neverStreamedBound,
		clock:              clock,
		reservations:       make(map[uint64]struct{}),
		leases:             make(map[string]*leaseRecord),
	}
}

func (m *LeaseManager) Reserve() (Reservation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.maxSessions > 0 && len(m.reservations)+len(m.leases) >= m.maxSessions {
		return Reservation{}, ErrLeaseCapacity
	}
	token := m.nextToken.Add(1)
	m.reservations[token] = struct{}{}
	return Reservation{token: token}, nil
}

func (m *LeaseManager) Commit(res Reservation, sessionID string) error {
	if sessionID == "" {
		return errors.New("mcp lease session id is empty")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.reservations[res.token]; !ok {
		return ErrReservationNotFound
	}
	if _, ok := m.leases[sessionID]; ok {
		return ErrLeaseExists
	}
	delete(m.reservations, res.token)
	now := m.clock.Now()
	m.leases[sessionID] = &leaseRecord{
		sessionID:    sessionID,
		safeID:       safeLeaseID(sessionID),
		state:        LeaseStateActive,
		createdAt:    now,
		lastActivity: now,
		reason:       "initialized",
	}
	return nil
}

func (m *LeaseManager) ReleaseReservation(res Reservation) {
	m.mu.Lock()
	delete(m.reservations, res.token)
	m.mu.Unlock()
}

func (m *LeaseManager) CapacityUsed() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.reservations) + len(m.leases)
}

// BeginRequest atomically admits an active session request and returns an
// idempotent completion guard. Only structurally classified application
// requests refresh lastActivity.
func (m *LeaseManager) BeginRequest(sessionID string, applicationActivity bool) (func(), error) {
	m.mu.Lock()
	lease := m.leases[sessionID]
	if lease == nil || lease.state != LeaseStateActive {
		m.mu.Unlock()
		return nil, ErrLeaseNotActive
	}
	lease.inFlight++
	if applicationActivity {
		now := m.clock.Now()
		lease.lastActivity = now
		if !lease.disconnectDeadline.IsZero() {
			lease.disconnectDeadline = now.Add(m.disconnectGrace)
		}
	}
	m.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			m.mu.Lock()
			defer m.mu.Unlock()
			current := m.leases[sessionID]
			if current != lease || current.inFlight == 0 {
				return
			}
			current.inFlight--
			if applicationActivity {
				now := m.clock.Now()
				current.lastActivity = now
				if !current.disconnectDeadline.IsZero() {
					current.disconnectDeadline = now.Add(m.disconnectGrace)
				}
			}
		})
	}, nil
}

func (m *LeaseManager) BeginSSE(sessionID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	lease := m.leases[sessionID]
	if lease == nil || lease.state != LeaseStateActive {
		return ErrLeaseNotActive
	}
	lease.sseCount++
	lease.everStreamed = true
	lease.disconnectDeadline = time.Time{}
	return nil
}

func (m *LeaseManager) EndSSE(sessionID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if lease := m.leases[sessionID]; lease != nil && lease.sseCount > 0 {
		lease.sseCount--
		if lease.sseCount == 0 && m.disconnectGrace > 0 {
			lease.disconnectDeadline = m.clock.Now().Add(m.disconnectGrace)
		}
	}
}

func (m *LeaseManager) InFlight(sessionID string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	if lease := m.leases[sessionID]; lease != nil {
		return lease.inFlight
	}
	return 0
}

// ExpireEligible atomically transitions eligible leases to expiring. Cleanup
// IO happens outside the manager; FinalizeClose releases capacity after proof.
func (m *LeaseManager) ExpireEligible() []ExpiredLease {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.clock.Now()
	var expired []ExpiredLease
	for id, lease := range m.leases {
		if lease.state != LeaseStateActive || lease.inFlight != 0 {
			continue
		}
		reason := ""
		if !lease.disconnectDeadline.IsZero() && !now.Before(lease.disconnectDeadline) {
			reason = "client_disconnected"
		} else if m.neverStreamedBound > 0 && !lease.everStreamed && now.Sub(lease.lastActivity) >= m.neverStreamedBound {
			reason = "never_streamed"
		} else if m.idleTimeout > 0 && now.Sub(lease.lastActivity) >= m.idleTimeout {
			reason = "idle_timeout"
		}
		if reason != "" {
			lease.state = LeaseStateExpiring
			lease.reason = reason
			expired = append(expired, ExpiredLease{SessionID: id, Reason: reason})
		}
	}
	sort.Slice(expired, func(i, j int) bool {
		return expired[i].SessionID < expired[j].SessionID
	})
	return expired
}

func (m *LeaseManager) TryBeginExpiry(sessionID, reason string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	lease := m.leases[sessionID]
	if lease == nil || lease.state != LeaseStateActive || lease.inFlight != 0 {
		return false
	}
	lease.state = LeaseStateExpiring
	lease.reason = reason
	return true
}

func (m *LeaseManager) MarkCleanupUncertain(sessionID, reason string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	lease := m.leases[sessionID]
	if lease == nil {
		return false
	}
	lease.state = LeaseStateCleanupUncertain
	lease.reason = reason
	return true
}

// RestoreActive reverses an expiry transition only when no cleanup request was
// dispatched. It is used for pre-dispatch backend admission failures.
func (m *LeaseManager) RestoreActive(sessionID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	lease := m.leases[sessionID]
	if lease == nil || lease.state != LeaseStateExpiring {
		return false
	}
	lease.state = LeaseStateActive
	lease.reason = ""
	return true
}

func (m *LeaseManager) FinalizeClose(sessionID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	lease, ok := m.leases[sessionID]
	if !ok {
		return false
	}
	m.recordClosedLocked(lease, lease.reason)
	delete(m.leases, sessionID)
	return true
}

// InvalidateAll clears every lease and pending reservation after the owning
// backend process or public listener has been torn down. No downstream session
// can remain reachable after that ownership boundary disappears.
func (m *LeaseManager) InvalidateAll(reason ...string) int {
	m.mu.Lock()
	closeReason := "backend_lost"
	if len(reason) > 0 && reason[0] != "" {
		closeReason = reason[0]
	}
	ids := make([]string, 0, len(m.leases))
	for id := range m.leases {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		m.recordClosedLocked(m.leases[id], closeReason)
	}
	closedCount := len(m.leases)
	m.reservations = make(map[uint64]struct{})
	m.leases = make(map[string]*leaseRecord)
	m.mu.Unlock()
	return closedCount
}

func (m *LeaseManager) recordClosedLocked(lease *leaseRecord, reason string) {
	if lease == nil {
		return
	}
	now := m.clock.Now()
	if reason == "" {
		reason = "closed"
	}
	row := LeaseSnapshotRow{
		SafeID:          lease.safeID,
		State:           LeaseStateClosed,
		Age:             now.Sub(lease.createdAt),
		Idle:            now.Sub(lease.lastActivity),
		InFlight:        lease.inFlight,
		SSEConnections:  lease.sseCount,
		LifecycleReason: reason,
	}
	if len(m.closed) == maxClosedLeaseHistory {
		copy(m.closed, m.closed[1:])
		m.closed[len(m.closed)-1] = row
		m.closedOmitted++
		return
	}
	m.closed = append(m.closed, row)
}

func (m *LeaseManager) AggregateInFlight() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	total := 0
	for _, lease := range m.leases {
		total += lease.inFlight
	}
	return total
}

func (m *LeaseManager) Snapshot(limit int) LeaseSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.clock.Now()
	rows := make([]LeaseSnapshotRow, 0, len(m.leases))
	for _, lease := range m.leases {
		rows = append(rows, LeaseSnapshotRow{
			SafeID:          lease.safeID,
			State:           lease.state,
			Age:             now.Sub(lease.createdAt),
			Idle:            now.Sub(lease.lastActivity),
			InFlight:        lease.inFlight,
			SSEConnections:  lease.sseCount,
			LifecycleReason: lease.reason,
		})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].SafeID < rows[j].SafeID })
	omitted := 0
	if limit > 0 && len(rows) > limit {
		omitted = len(rows) - limit
		rows = rows[:limit]
	}
	return LeaseSnapshot{
		CapacityUsed:  len(m.reservations) + len(m.leases),
		CapacityMax:   m.maxSessions,
		Rows:          rows,
		Omitted:       omitted,
		Closed:        append([]LeaseSnapshotRow(nil), m.closed...),
		ClosedOmitted: m.closedOmitted,
	}
}

func safeLeaseID(sessionID string) string {
	sum := sha256.Sum256([]byte(sessionID))
	return hex.EncodeToString(sum[:6])
}

// ClassifyApplicationActivity returns true only when a JSON-RPC envelope
// contains a client request that is not protocol-maintenance ping traffic.
func ClassifyApplicationActivity(body []byte) (bool, error) {
	if len(body) == 0 {
		return false, errors.New("empty JSON-RPC envelope")
	}
	var raw json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return false, fmt.Errorf("decode JSON-RPC envelope: %w", err)
	}
	if len(raw) == 0 {
		return false, errors.New("empty JSON-RPC envelope")
	}
	if raw[0] == '[' {
		var batch []json.RawMessage
		if err := json.Unmarshal(raw, &batch); err != nil {
			return false, fmt.Errorf("decode JSON-RPC batch: %w", err)
		}
		if len(batch) == 0 {
			return false, errors.New("empty JSON-RPC batch")
		}
		active := false
		for _, member := range batch {
			memberActive, err := classifyJSONRPCMessage(member)
			if err != nil {
				return false, err
			}
			active = active || memberActive
		}
		return active, nil
	}
	return classifyJSONRPCMessage(raw)
}

func classifyJSONRPCMessage(raw json.RawMessage) (bool, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return false, fmt.Errorf("decode JSON-RPC message: %w", err)
	}
	if len(fields) == 0 {
		return false, errors.New("empty JSON-RPC message")
	}
	methodRaw, hasMethod := fields["method"]
	_, hasID := fields["id"]
	_, hasResult := fields["result"]
	_, hasError := fields["error"]
	if hasMethod {
		var method string
		if err := json.Unmarshal(methodRaw, &method); err != nil || method == "" {
			return false, errors.New("invalid JSON-RPC method")
		}
		if !hasID { // notification
			return false, nil
		}
		return method != "ping", nil
	}
	if hasID && (hasResult || hasError) { // response
		return false, nil
	}
	return false, errors.New("unrecognized JSON-RPC message")
}
