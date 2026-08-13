package ownership

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
)

// ParseProcStat parses Linux /proc/PID/stat. The command name may contain
// spaces and right parentheses, so the final right parenthesis is the record
// boundary; fields are then indexed relative to the state field.
func ParseProcStat(data []byte) (ProcStat, error) {
	text := strings.TrimSpace(string(data))
	open := strings.IndexByte(text, '(')
	close := strings.LastIndexByte(text, ')')
	if open <= 0 || close <= open || close+1 >= len(text) {
		return ProcStat{}, fmt.Errorf("malformed proc stat")
	}
	pid, err := strconv.Atoi(strings.TrimSpace(text[:open]))
	if err != nil || pid <= 0 {
		return ProcStat{}, fmt.Errorf("malformed proc pid")
	}
	fields := strings.Fields(text[close+1:])
	// fields[0] is state; pgrp is field 5 and starttime is field 22 in
	// proc(5), hence offsets 2 and 19 after the command name.
	if len(fields) <= 19 || len(fields[0]) != 1 {
		return ProcStat{}, fmt.Errorf("malformed proc stat fields")
	}
	pgrp, err := strconv.Atoi(fields[2])
	if err != nil || pgrp <= 0 {
		return ProcStat{}, fmt.Errorf("malformed proc process group")
	}
	start, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil || start == 0 {
		return ProcStat{}, fmt.Errorf("malformed proc start time")
	}
	return ProcStat{PID: pid, PGRP: pgrp, StartTime: start}, nil
}

// ParseEnviron returns the value for an exact NUL-delimited environment key.
func ParseEnviron(data []byte, key string) (string, bool) {
	if key == "" || strings.ContainsAny(key, "=\x00") {
		return "", false
	}
	for _, entry := range bytes.Split(data, []byte{0}) {
		name, value, ok := strings.Cut(string(entry), "=")
		if ok && name == key {
			return value, true
		}
	}
	return "", false
}

// VerifyGroup checks the boot, configuration, leader/member process identity,
// and owner-token hash without taking any action or emitting signals.
func VerifyGroup(reader ProcReader, lease Lease, identity ServerIdentity) (GroupStatus, error) {
	if reader == nil {
		return GroupUnknown, nil
	}
	if lease.Version != LeaseSchemaVersion || lease.OwnerTokenHash == "" || lease.ConfigHash == "" {
		return GroupUnknown, nil
	}
	boot, err := reader.ReadBootID()
	if err != nil {
		return GroupUnknown, nil
	}
	if boot != lease.BootID {
		return GroupUnknown, nil
	}
	if ConfigHash(identity) != lease.ConfigHash {
		return GroupConflict, nil
	}

	leader, leaderErr := reader.ReadStat(lease.LeaderPID)
	leaderLive := leaderErr == nil
	if leaderLive {
		if leader.PID != lease.LeaderPID || leader.PGRP != lease.LeaderPGID || leader.StartTime != lease.LeaderStart {
			return GroupConflict, nil
		}
		exe, err := reader.ReadExe(lease.LeaderPID)
		if err != nil {
			return GroupUnknown, nil
		}
		if lease.Executable != "" && exe != lease.Executable {
			return GroupConflict, nil
		}
	}

	members, err := reader.ListMembers(lease.LeaderPGID)
	if err != nil {
		return GroupUnknown, nil
	}
	if len(members) == 0 {
		if leaderLive {
			return GroupUnknown, nil
		}
		return GroupDead, nil
	}

	seenLeader := false
	for _, pid := range members {
		stat, err := reader.ReadStat(pid)
		if err != nil {
			return GroupUnknown, nil
		}
		if stat.PID != pid || stat.PGRP != lease.LeaderPGID {
			return GroupConflict, nil
		}
		if stat.StartTime < lease.LeaderStart {
			return GroupConflict, nil
		}
		env, err := reader.ReadEnviron(pid)
		if err != nil {
			return GroupUnknown, nil
		}
		token, ok := ParseEnviron(env, OwnerTokenEnvKey)
		if !ok || token == "" || TokenHash(token) != lease.OwnerTokenHash {
			return GroupConflict, nil
		}
		if pid == lease.LeaderPID {
			seenLeader = true
		}
	}
	if leaderLive && !seenLeader {
		return GroupConflict, nil
	}
	return GroupOwned, nil
}
