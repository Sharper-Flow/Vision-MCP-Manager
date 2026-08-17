// Package reachability tracks probe-backed reachability independently from
// managed-server lifecycle and admission state.
package reachability

import (
	"strings"
	"sync"
	"time"
	"unicode"
)

// State is the current reachability state of a server.
type State string

const (
	StateUnprobed    State = "unprobed"
	StateProbing     State = "probing"
	StateReachable   State = "reachable"
	StateUnreachable State = "unreachable"
)

// Depth identifies how far a probe got before producing evidence.
type Depth string

const (
	DepthListener Depth = "listener"
	DepthSession  Depth = "session"
	DepthEndToEnd Depth = "end_to_end"
)

// Rank orders probe depths from shallowest to deepest for evidence selection.
func (d Depth) Rank() int {
	switch d {
	case DepthListener:
		return 1
	case DepthSession:
		return 2
	case DepthEndToEnd:
		return 3
	default:
		return 0
	}
}

// Outcome is the result of a completed probe.
type Outcome string

const (
	OutcomeSuccess Outcome = "success"
	OutcomeFailure Outcome = "failure"
)

const (
	// FailureThreshold prevents transient failures from reporting a server as
	// unreachable. This is the daemon-wide established probe convention.
	FailureThreshold = 3
	maxErrorLength   = 1024
)

// ProbeEvidence is the latest result and failure streak for one probe depth.
// LastProbeError is deliberately a bounded, single-line string so it can be
// rendered directly by administrative surfaces without exposing an error
// object or control characters.
type ProbeEvidence struct {
	Depth               Depth     `json:"depth"`
	LastProbeAttempt    time.Time `json:"last_probe_attempt"`
	LastProbeOutcome    Outcome   `json:"last_probe_outcome"`
	LastProbeError      string    `json:"last_probe_error,omitempty"`
	ConsecutiveFailures int       `json:"consecutive_failures"`
}

// Reachability is a point-in-time value returned by Store. Evidence is kept
// per depth so a shallow result cannot erase a deeper result. Callers receive
// an independent copy of the evidence map.
type Reachability struct {
	State    State                   `json:"state"`
	Evidence map[Depth]ProbeEvidence `json:"evidence,omitempty"`
}

// ProbeResult records one completed probe. A zero AttemptedAt is replaced by
// the store's current time. Error is ignored for successful probes.
type ProbeResult struct {
	Depth       Depth
	AttemptedAt time.Time
	Success     bool
	Error       string
}

type serverRecord struct {
	reachability Reachability
	stateByDepth map[Depth]State
	probing      map[Depth]int
}

// Store is a concurrency-safe, in-memory reachability store keyed by server
// name. It has no admission or lifecycle behavior.
type Store struct {
	mu      sync.RWMutex
	servers map[string]*serverRecord
}

// NewStore creates an empty reachability store.
func NewStore() *Store {
	return &Store{servers: make(map[string]*serverRecord)}
}

// StartProbe marks a server as probing at depth and records the attempt time.
// It is safe for multiple probe workers to start concurrently.
func (s *Store) StartProbe(serverName string, depth Depth, attemptedAt time.Time) Reachability {
	validateDepth(depth)
	if attemptedAt.IsZero() {
		attemptedAt = time.Now()
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	record := s.ensureRecord(serverName)
	record.probing[depth]++
	evidence := record.reachability.Evidence[depth]
	evidence.Depth = depth
	evidence.LastProbeAttempt = attemptedAt
	record.reachability.Evidence[depth] = evidence
	record.reachability.State = stateFor(record)
	return cloneReachability(record.reachability)
}

// RecordProbe records a completed probe and returns the resulting value.
// Failures become unreachable only after FailureThreshold consecutive failures
// at the same depth. A success resets that depth's failure streak.
func (s *Store) RecordProbe(serverName string, result ProbeResult) Reachability {
	validateDepth(result.Depth)
	if result.AttemptedAt.IsZero() {
		result.AttemptedAt = time.Now()
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	record := s.ensureRecord(serverName)
	if record.probing[result.Depth] > 0 {
		record.probing[result.Depth]--
	}

	evidence := record.reachability.Evidence[result.Depth]
	evidence.Depth = result.Depth
	evidence.LastProbeAttempt = result.AttemptedAt
	if result.Success {
		evidence.LastProbeOutcome = OutcomeSuccess
		evidence.LastProbeError = ""
		evidence.ConsecutiveFailures = 0
		record.stateByDepth[result.Depth] = StateReachable
	} else {
		evidence.LastProbeOutcome = OutcomeFailure
		evidence.LastProbeError = safeError(result.Error)
		evidence.ConsecutiveFailures++
		if evidence.ConsecutiveFailures >= FailureThreshold {
			record.stateByDepth[result.Depth] = StateUnreachable
		} else if _, ok := record.stateByDepth[result.Depth]; !ok {
			record.stateByDepth[result.Depth] = StateUnprobed
		}
	}
	record.reachability.Evidence[result.Depth] = evidence
	record.reachability.State = stateFor(record)
	return cloneReachability(record.reachability)
}

// RecordDefinitiveFailure records a failure that is known to be terminal rather
// than transient, marking the depth unreachable immediately without waiting for
// FailureThreshold consecutive failures.
//
// FailureThreshold exists to stop a flaky probe from reporting a healthy server
// as broken. That reasoning does not apply to a failure whose cause is already
// proven, such as a proxy listener that failed to register: there is no listener
// to become reachable again, so requiring two further confirmations would report
// a known-broken server as healthy for the duration.
//
// Callers must use this only when the failure is structurally terminal. A probe
// that merely failed to connect is transient and belongs in RecordProbe.
func (s *Store) RecordDefinitiveFailure(serverName string, depth Depth, attemptedAt time.Time, cause string) Reachability {
	validateDepth(depth)
	if attemptedAt.IsZero() {
		attemptedAt = time.Now()
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	record := s.ensureRecord(serverName)
	if record.probing[depth] > 0 {
		record.probing[depth]--
	}

	evidence := record.reachability.Evidence[depth]
	evidence.Depth = depth
	evidence.LastProbeAttempt = attemptedAt
	evidence.LastProbeOutcome = OutcomeFailure
	evidence.LastProbeError = safeError(cause)
	// Report the single failure that actually occurred. Inflating this counter
	// to trip the threshold would misreport how many attempts were made in the
	// same admin payload operators rely on to diagnose the outage.
	evidence.ConsecutiveFailures++
	record.stateByDepth[depth] = StateUnreachable

	record.reachability.Evidence[depth] = evidence
	record.reachability.State = stateFor(record)
	return cloneReachability(record.reachability)
}

// Get returns the current reachability for serverName. An unseen server
// returns an unprobed value and false.
func (s *Store) Get(serverName string) (Reachability, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	record, ok := s.servers[serverName]
	if !ok {
		return Reachability{State: StateUnprobed}, false
	}
	return cloneReachability(record.reachability), true
}

// Remove deletes all reachability evidence for serverName.
func (s *Store) Remove(serverName string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.servers, serverName)
}

func (s *Store) ensureRecord(serverName string) *serverRecord {
	if record, ok := s.servers[serverName]; ok {
		return record
	}
	record := &serverRecord{
		reachability: Reachability{
			State:    StateUnprobed,
			Evidence: make(map[Depth]ProbeEvidence),
		},
		stateByDepth: make(map[Depth]State),
		probing:      make(map[Depth]int),
	}
	s.servers[serverName] = record
	return record
}

func stateFor(record *serverRecord) State {
	if len(record.reachability.Evidence) == 0 {
		return StateUnprobed
	}

	// Unreachable at ANY depth dominates, and this asymmetry is deliberate.
	// Depth describes how far a probe reached, not how authoritative it is, and
	// evidence at different depths is gathered at different times. A deeper
	// success can therefore be stale: an end-to-end probe may have succeeded
	// minutes ago while the listener has since stopped answering. Preferring the
	// deeper result there would report reachable for a server no client can
	// connect to — the exact failure this package exists to make impossible.
	//
	// Reachability is pessimistic: any depth that has crossed the failure
	// threshold makes the server unreachable, regardless of depth ordering.
	for _, depthState := range record.stateByDepth {
		if depthState == StateUnreachable {
			return StateUnreachable
		}
	}
	for _, count := range record.probing {
		if count > 0 {
			return StateProbing
		}
	}

	// No depth is failing; the deepest evidence is the most informative.
	deepest := DepthListener
	for depth := range record.reachability.Evidence {
		if depth.Rank() > deepest.Rank() {
			deepest = depth
		}
	}
	return record.stateByDepth[deepest]
}

func cloneReachability(value Reachability) Reachability {
	clone := Reachability{State: value.State, Evidence: make(map[Depth]ProbeEvidence, len(value.Evidence))}
	for depth, evidence := range value.Evidence {
		clone.Evidence[depth] = evidence
	}
	return clone
}

func validateDepth(depth Depth) {
	if depth.Rank() == 0 {
		panic("reachability: invalid probe depth")
	}
}

func safeError(value string) string {
	value = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, value)
	value = strings.TrimSpace(value)
	runes := []rune(value)
	if len(runes) > maxErrorLength {
		return string(runes[:maxErrorLength])
	}
	return value
}
