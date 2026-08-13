package admin

import (
	"strings"

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
	Status string
	Reason string
}

// deriveEffectiveStatus is the single precedence table for process and
// managed-backend status exposed by admin surfaces.
func deriveEffectiveStatus(process server.State, backend, rawReason string) EffectiveStatus {
	var status, reason string
	switch process {
	case server.StateFailed, server.StateCrashed:
		status, reason = "error", rawReason
		if reason == "" {
			reason = "process " + string(process)
		}
	case server.StateStopped, server.StateStopping:
		status = "stopped"
	case server.StateStarting:
		status = "starting"
	case server.StateRunning:
		switch strings.ToLower(backend) {
		case "", "ready":
			status = "running"
		case "starting", "probing":
			status = "starting"
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
	return EffectiveStatus{Status: status, Reason: scrubSecrets(reason)}
}
