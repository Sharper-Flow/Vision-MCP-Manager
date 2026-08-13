package ownership

import "time"

// LeaseSchemaVersion is the on-disk lease format version.
const LeaseSchemaVersion = 1

// OwnerTokenEnvKey is the only environment key used for backend ownership.
const OwnerTokenEnvKey = "VISION_BACKEND_OWNER_TOKEN"

// Lease contains the durable, non-secret identity of a managed backend.
// The raw owner token is deliberately not part of this type.
type Lease struct {
	Version        int    `json:"version"`
	Generation     uint64 `json:"generation"`
	ServerName     string `json:"server_name"`
	DaemonID       string `json:"daemon_id"`
	OwnerTokenHash string `json:"owner_token_hash"`
	ConfigHash     string `json:"config_hash"`
	LeaderPID      int    `json:"leader_pid"`
	LeaderPGID     int    `json:"leader_pgid"`
	// LeaderStart is the process-group generation lower bound in proc start
	// ticks. CreatedAt is audit metadata and is not comparable to proc ticks.
	LeaderStart uint64    `json:"leader_start"`
	BootID      string    `json:"boot_id"`
	Executable  string    `json:"executable,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

// ServerIdentity is the explicit configuration identity used in a lease.
// Env is intentionally excluded from ConfigHash: ownership is not based on
// arbitrary child environment values.
type ServerIdentity struct {
	Name      string
	Command   string
	Args      []string
	URL       string
	Transport string
	Env       map[string]string
}

// ProcStat is the subset of /proc/PID/stat needed for ownership checks.
type ProcStat struct {
	PID       int
	PGRP      int
	StartTime uint64
}

// ProcInfo is a convenient representation for proc readers and fakes.
type ProcInfo struct {
	Exe     string
	Environ []byte
}

// ProcReader supplies process identity information to VerifyGroup.
type ProcReader interface {
	ReadStat(pid int) (ProcStat, error)
	ReadEnviron(pid int) ([]byte, error)
	ReadExe(pid int) (string, error)
	ReadBootID() (string, error)
	ListMembers(pgid int) ([]int, error)
}

// GroupInspector is the minimum process-group lifecycle contract used by
// supervisors when the recorded leader has already exited.
type GroupInspector interface {
	ListMembers(pgid int) ([]int, error)
}

// LeaseStore is the narrow lifecycle contract consumed by supervision.
type LeaseStore interface {
	Record(serverName string, lease Lease) error
	Read(serverName string) (Lease, error)
	ReleaseGeneration(serverName string, generation uint64) error
}

// GroupStatus is the result of a fail-closed ownership check.
type GroupStatus string

const (
	GroupOwned    GroupStatus = "owned"
	GroupDead     GroupStatus = "dead"
	GroupConflict GroupStatus = "conflict"
	GroupUnknown  GroupStatus = "unknown"
)
