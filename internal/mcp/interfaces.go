package mcp

import (
	"context"

	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/session"
)

// AdmissionStatuser abstracts admission-control state for fixed and multiplexed
// session managers.
type AdmissionStatuser interface {
	AdmissionStatus() (atCapacity bool, current int, max int)
}

// ManagerSelector chooses which concrete session manager should own a new
// upstream session. Used by slot groups for transparent routing.
type ManagerSelector interface {
	SessionCloser
	AdmissionStatuser
	SelectForNewSession(ctx context.Context, upstreamSessionID string) (*session.Manager, error)
	Rebind(oldKey, newKey string)
	ReportSpawnResult(sessionKey string, err error)
	SetOnSessionRemoved(fn func(sessionID string))
	Release(upstreamSessionID string)
}
