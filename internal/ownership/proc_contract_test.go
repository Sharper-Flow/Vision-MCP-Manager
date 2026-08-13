package ownership

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestParseProcStatHandlesParenthesesAndRejectsMalformedInput(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		input     string
		wantPID   int
		wantPGRP  int
		wantStart uint64
		wantErr   bool
	}{
		{
			name:      "spaces and right parentheses in command name",
			input:     "321 (worker with spaces ))) ) S 11 654 " + strings.Repeat("0 ", 16) + "987654 0",
			wantPID:   321,
			wantPGRP:  654,
			wantStart: 987654,
		},
		{
			name:    "missing stat fields",
			input:   "321 (worker) S 11",
			wantErr: true,
		},
		{
			name:    "malformed pid",
			input:   "pid (worker) S 11 654",
			wantErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseProcStat([]byte(tc.input))
			if tc.wantErr {
				if err == nil {
					t.Fatal("ParseProcStat() error = nil, want malformed-input error")
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseProcStat() error = %v", err)
			}
			if got.PID != tc.wantPID || got.PGRP != tc.wantPGRP || got.StartTime != tc.wantStart {
				t.Fatalf("ParseProcStat() = %#v, want pid=%d pgrp=%d start=%d", got, tc.wantPID, tc.wantPGRP, tc.wantStart)
			}
		})
	}
}

func TestParseEnvironUsesExactNULDelimitedKeys(t *testing.T) {
	t.Parallel()
	environ := []byte("VISION_OWNER=token-a\x00VISION_OWNER_EXTRA=token-b\x00EMPTY=\x00")
	if got, ok := ParseEnviron(environ, "VISION_OWNER"); !ok || got != "token-a" {
		t.Fatalf("ParseEnviron(exact key) = %q, %v", got, ok)
	}
	if got, ok := ParseEnviron(environ, "OWNER"); ok || got != "" {
		t.Fatalf("ParseEnviron(substring key) = %q, %v, want absent", got, ok)
	}
	if got, ok := ParseEnviron(environ, "EMPTY"); !ok || got != "" {
		t.Fatalf("ParseEnviron(empty value) = %q, %v", got, ok)
	}
}

type fakeProcReader struct {
	bootID  string
	bootErr error
	procs   map[int]ProcInfo
	stats   map[int]ProcStat
	statErr map[int]error
	member  map[int][]int
}

func (f *fakeProcReader) ReadStat(pid int) (ProcStat, error) {
	if err := f.statErr[pid]; err != nil {
		return ProcStat{}, err
	}
	stat, ok := f.stats[pid]
	if !ok {
		return ProcStat{}, errors.New("missing fake stat")
	}
	return stat, nil
}

func (f *fakeProcReader) ReadEnviron(pid int) ([]byte, error) {
	proc, ok := f.procs[pid]
	if !ok {
		return nil, errors.New("missing fake environ")
	}
	return proc.Environ, nil
}

func (f *fakeProcReader) ReadExe(pid int) (string, error) {
	proc, ok := f.procs[pid]
	if !ok {
		return "", errors.New("missing fake exe")
	}
	return proc.Exe, nil
}

func (f *fakeProcReader) ReadBootID() (string, error) { return f.bootID, f.bootErr }

func (f *fakeProcReader) ListMembers(pgid int) ([]int, error) {
	return f.member[pgid], nil
}

func TestVerifyGroupNeverClaimsUnsafeOrUnknownGroupsOwned(t *testing.T) {
	t.Parallel()
	const token = "owner-token"
	baseLease := Lease{
		Version:        LeaseSchemaVersion,
		Generation:     4,
		ServerName:     "playwright",
		OwnerTokenHash: TokenHash(token),
		ConfigHash:     ConfigHash(ServerIdentity{Name: "playwright", Command: "/vision/browser", Transport: "managed-http"}),
		LeaderPID:      100,
		LeaderPGID:     100,
		LeaderStart:    500,
		BootID:         "boot-a",
		Executable:     "/vision/browser",
	}
	baseIdentity := ServerIdentity{Name: "playwright", Command: "/vision/browser", Transport: "managed-http"}

	newReader := func() *fakeProcReader {
		return &fakeProcReader{
			bootID: "boot-a",
			procs: map[int]ProcInfo{
				100: {Exe: "/vision/browser", Environ: []byte("VISION_BACKEND_OWNER_TOKEN=" + token + "\x00")},
				101: {Exe: "/vision/browser", Environ: []byte("VISION_BACKEND_OWNER_TOKEN=" + token + "\x00")},
			},
			stats: map[int]ProcStat{
				100: {PID: 100, PGRP: 100, StartTime: 500},
				101: {PID: 101, PGRP: 100, StartTime: 501},
			},
			statErr: map[int]error{},
			member:  map[int][]int{100: {100, 101}},
		}
	}

	tests := []struct {
		name  string
		setup func(*fakeProcReader, *Lease, *ServerIdentity)
		owned bool
		dead  bool
	}{
		{name: "live leader and matching descendants", owned: true},
		{
			name:  "dead leader with descendants from later generation start",
			owned: true,
			setup: func(f *fakeProcReader, lease *Lease, _ *ServerIdentity) {
				f.statErr[100] = errors.New("leader exited")
				delete(f.procs, 100)
				f.member[100] = []int{101}
				lease.LeaderStart = 500
			},
		},
		{
			name: "empty group is dead",
			dead: true,
			setup: func(f *fakeProcReader, _ *Lease, _ *ServerIdentity) {
				f.statErr[100] = errors.New("leader exited")
				f.member[100] = nil
			},
		},
		{
			name: "mixed owner token is conflict",
			setup: func(f *fakeProcReader, _ *Lease, _ *ServerIdentity) {
				f.procs[101] = ProcInfo{Exe: "/vision/browser", Environ: []byte("VISION_BACKEND_OWNER_TOKEN=other\x00")}
			},
		},
		{
			name: "old descendant start is conflict",
			setup: func(f *fakeProcReader, _ *Lease, _ *ServerIdentity) {
				f.stats[101] = ProcStat{PID: 101, PGRP: 100, StartTime: 499}
			},
		},
		{
			name: "unreadable member is unknown",
			setup: func(f *fakeProcReader, _ *Lease, _ *ServerIdentity) {
				f.statErr[101] = errors.New("permission denied")
			},
		},
		{
			name:  "boot mismatch is unknown",
			setup: func(f *fakeProcReader, _ *Lease, _ *ServerIdentity) { f.bootID = "boot-b" },
		},
		{
			name: "live leader executable mismatch is conflict",
			setup: func(f *fakeProcReader, _ *Lease, _ *ServerIdentity) {
				f.procs[100] = ProcInfo{Exe: "/other", Environ: []byte("VISION_BACKEND_OWNER_TOKEN=" + token + "\x00")}
			},
		},
		{
			name: "config mismatch is conflict",
			setup: func(_ *fakeProcReader, _ *Lease, identity *ServerIdentity) {
				identity.Command = "/other-config"
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			reader := newReader()
			lease := baseLease
			identity := baseIdentity
			if tc.setup != nil {
				tc.setup(reader, &lease, &identity)
			}
			status, err := VerifyGroup(reader, lease, identity)
			if err != nil {
				t.Fatalf("VerifyGroup() error = %v", err)
			}
			if (status == GroupOwned) != tc.owned {
				t.Fatalf("VerifyGroup() status = %v, owned = %v", status, tc.owned)
			}
			if tc.dead && status != GroupDead {
				t.Fatalf("VerifyGroup() status = %v, want dead", status)
			}
		})
	}
}

func TestFakeProcReaderSatisfiesPlatformNeutralVerifierContract(t *testing.T) {
	t.Parallel()
	var _ ProcReader = (*fakeProcReader)(nil)
	if fmt.Sprint(GroupOwned) == "" {
		t.Fatal("group status constants must be printable")
	}
}
