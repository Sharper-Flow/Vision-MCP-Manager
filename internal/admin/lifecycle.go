package admin

import (
	"fmt"
	"strings"
	"time"

	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/reachability"
	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/server"
)

// SessionLifecycleRow is a secret-safe immutable projection of one managed
// native-HTTP session. SafeID is a bounded hash, never the MCP session ID.
type SessionLifecycleRow struct {
	SafeID                 string `json:"safe_id"`
	State                  string `json:"state"`
	AgeSeconds             int64  `json:"age_seconds"`
	ApplicationIdleSeconds int64  `json:"application_idle_seconds"`
	InFlight               int    `json:"in_flight"`
	SSEConnections         int    `json:"sse_connections"`
	LifecycleReason        string `json:"lifecycle_reason"`
}

// SessionLifecycleSnapshot is bounded by the lease manager: at most 100 active
// rows are projected and at most 1,000 closed rows are retained.
type SessionLifecycleSnapshot struct {
	BackendState  string                `json:"backend_state"`
	CapacityUsed  int                   `json:"capacity_used"`
	CapacityMax   int                   `json:"capacity_max"`
	Sessions      []SessionLifecycleRow `json:"sessions"`
	Closed        []SessionLifecycleRow `json:"closed"`
	Omitted       int                   `json:"omitted"`
	ClosedOmitted int                   `json:"closed_omitted"`
}

type SessionLifecycleAccessor interface {
	SessionLifecycleSnapshot(serverName string) *SessionLifecycleSnapshot
}

type EffectiveStatus struct {
	Status       string
	Reason       string
	Reachability ReachabilityDetails
}

// ReachabilityDetails is the additive, flat projection shared by every admin
// status response. A nil last_probe_at is encoded as JSON null when no probe
// has completed yet; the remaining fields retain stable zero values.
type ReachabilityDetails struct {
	Reachability             string     `json:"reachability"`
	ProbeDepth               string     `json:"probe_depth"`
	LastProbeAt              *time.Time `json:"last_probe_at"`
	LastProbeOutcome         string     `json:"last_probe_outcome"`
	LastProbeError           string     `json:"last_probe_error"`
	ConsecutiveProbeFailures int        `json:"consecutive_probe_failures"`
}

const DefaultReachabilityGrace = 30 * time.Second

// deriveEffectiveStatus is the single precedence table for process and
// managed-backend status exposed by admin surfaces.
func deriveEffectiveStatus(process server.State, backend string, value reachability.Reachability, processUptime, startupGrace time.Duration, rawReason string) EffectiveStatus {
	details := projectReachability(value)
	var status, reason string
	switch process {
	case server.StateFailed, server.StateCrashed:
		status, reason = "error", rawReason
		if reason == "" {
			reason = "process " + string(process)
		}
	case server.StateStopped, server.StateStopping:
		status, reason = "stopped", "process "+string(process)
	case server.StateStarting:
		status, reason = "starting", "process starting"
	case server.StateRunning:
		switch strings.ToLower(backend) {
		case "", "ready":
			reachabilityState := normalizedReachabilityState(value)
			switch reachabilityState {
			case reachability.StateReachable:
				status = "running"
			case reachability.StateProbing:
				status, reason = "starting", "awaiting first reachability probe"
			case reachability.StateUnprobed:
				if len(value.Evidence) == 0 && processUptime > startupGrace {
					status, reason = "error", fmt.Sprintf("no reachability evidence for %s", processUptime.Round(time.Second))
				} else {
					status, reason = "starting", "awaiting first reachability probe"
				}
			case reachability.StateUnreachable:
				status, reason = "error", details.LastProbeError
				if reason == "" {
					reason = "probe failure at " + details.ProbeDepth
				}
			default:
				status, reason = "error", "unknown reachability state"
			}
		case "starting", "probing":
			status, reason = "starting", "awaiting first reachability probe"
		case "draining", "recycling", "restarting":
			status, reason = "error", "backend "+strings.ToLower(backend)
		default:
			status, reason = "error", "unknown backend state"
		}
	default:
		status, reason = "error", "unknown process state"
	}
	if status == "error" && reason == "" {
		reason = rawReason
	}
	return EffectiveStatus{Status: status, Reason: scrubSecrets(reason), Reachability: details}
}

func normalizedReachabilityState(value reachability.Reachability) reachability.State {
	state := value.State
	if state == "" || ((state == reachability.StateReachable || state == reachability.StateUnreachable) && len(value.Evidence) == 0) {
		return reachability.StateUnprobed
	}
	return state
}

func projectReachability(value reachability.Reachability) ReachabilityDetails {
	details := ReachabilityDetails{Reachability: string(normalizedReachabilityState(value))}
	evidence, ok := selectProbeEvidence(value)
	if !ok {
		return details
	}
	details.ProbeDepth = string(evidence.Depth)
	if !evidence.LastProbeAttempt.IsZero() {
		attempt := evidence.LastProbeAttempt
		details.LastProbeAt = &attempt
	}
	details.LastProbeOutcome = string(evidence.LastProbeOutcome)
	details.LastProbeError = scrubSecrets(evidence.LastProbeError)
	details.ConsecutiveProbeFailures = evidence.ConsecutiveFailures
	return details
}

func selectProbeEvidence(value reachability.Reachability) (reachability.ProbeEvidence, bool) {
	var selected reachability.ProbeEvidence
	selectedSet := false
	for _, evidence := range value.Evidence {
		if !selectedSet || probeEvidencePreferred(value.State, evidence, selected) {
			selected = evidence
			selectedSet = true
		}
	}
	return selected, selectedSet
}

func probeEvidencePreferred(state reachability.State, candidate, selected reachability.ProbeEvidence) bool {
	candidateFailure := state == reachability.StateUnreachable && candidate.ConsecutiveFailures >= reachability.FailureThreshold
	selectedFailure := state == reachability.StateUnreachable && selected.ConsecutiveFailures >= reachability.FailureThreshold
	if candidateFailure != selectedFailure {
		return candidateFailure
	}
	if !candidate.LastProbeAttempt.Equal(selected.LastProbeAttempt) {
		return candidate.LastProbeAttempt.After(selected.LastProbeAttempt)
	}
	return probeDepthRank(candidate.Depth) > probeDepthRank(selected.Depth)
}

func probeDepthRank(depth reachability.Depth) int {
	switch depth {
	case reachability.DepthListener:
		return 1
	case reachability.DepthSession:
		return 2
	case reachability.DepthEndToEnd:
		return 3
	default:
		return 0
	}
}

func (s *Server) reachabilityFor(name string) reachability.Reachability {
	if s.reachabilityStore == nil {
		return reachability.Reachability{State: reachability.StateUnprobed}
	}
	value, _ := s.reachabilityStore.Get(name)
	return value
}
