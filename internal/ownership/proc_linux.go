//go:build linux

package ownership

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

// LinuxProcReader reads process identity from the Linux proc filesystem.
type LinuxProcReader struct{ Root string }

// NewLinuxProcReader returns a reader rooted at /proc, or root when supplied.
func NewLinuxProcReader(root string) *LinuxProcReader {
	if root == "" {
		root = "/proc"
	}
	return &LinuxProcReader{Root: root}
}

func (r *LinuxProcReader) ReadStat(pid int) (ProcStat, error) {
	if pid <= 0 {
		return ProcStat{}, fmt.Errorf("invalid pid")
	}
	b, err := os.ReadFile(filepath.Join(r.Root, strconv.Itoa(pid), "stat"))
	if err != nil {
		return ProcStat{}, err
	}
	stat, err := ParseProcStat(b)
	if err != nil {
		return ProcStat{}, err
	}
	if stat.PID != pid {
		return ProcStat{}, fmt.Errorf("proc pid mismatch")
	}
	return stat, nil
}

func (r *LinuxProcReader) ReadEnviron(pid int) ([]byte, error) {
	if pid <= 0 {
		return nil, fmt.Errorf("invalid pid")
	}
	return os.ReadFile(filepath.Join(r.Root, strconv.Itoa(pid), "environ"))
}

func (r *LinuxProcReader) ReadExe(pid int) (string, error) {
	if pid <= 0 {
		return "", fmt.Errorf("invalid pid")
	}
	return os.Readlink(filepath.Join(r.Root, strconv.Itoa(pid), "exe"))
}

func (r *LinuxProcReader) ReadBootID() (string, error) {
	b, err := os.ReadFile(filepath.Join(r.Root, "sys", "kernel", "random", "boot_id"))
	if err != nil {
		return "", err
	}
	return string(bytesTrimSpace(b)), nil
}

func (r *LinuxProcReader) ListMembers(pgid int) ([]int, error) {
	if pgid <= 0 {
		return nil, fmt.Errorf("invalid process group")
	}
	entries, err := os.ReadDir(r.Root)
	if err != nil {
		return nil, err
	}
	members := make([]int, 0)
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 || !entry.IsDir() {
			continue
		}
		stat, err := r.ReadStat(pid)
		if err == nil && stat.PGRP == pgid {
			members = append(members, pid)
		}
	}
	return members, nil
}

func bytesTrimSpace(b []byte) []byte {
	start, end := 0, len(b)
	for start < end && (b[start] == ' ' || b[start] == '\n' || b[start] == '\r' || b[start] == '\t') {
		start++
	}
	for end > start && (b[end-1] == ' ' || b[end-1] == '\n' || b[end-1] == '\r' || b[end-1] == '\t') {
		end--
	}
	return b[start:end]
}
