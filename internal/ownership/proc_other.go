//go:build !linux

package ownership

import "errors"

// LinuxProcReader is unavailable on non-Linux systems; callers receive
// fail-closed errors rather than a platform-specific approximation.
type LinuxProcReader struct{ Root string }

func NewLinuxProcReader(root string) *LinuxProcReader { return &LinuxProcReader{Root: root} }
func (*LinuxProcReader) ReadStat(int) (ProcStat, error) {
	return ProcStat{}, errors.New("proc ownership unavailable")
}
func (*LinuxProcReader) ReadEnviron(int) ([]byte, error) {
	return nil, errors.New("proc ownership unavailable")
}
func (*LinuxProcReader) ReadExe(int) (string, error) {
	return "", errors.New("proc ownership unavailable")
}
func (*LinuxProcReader) ReadBootID() (string, error) {
	return "", errors.New("proc ownership unavailable")
}
func (*LinuxProcReader) ListMembers(int) ([]int, error) {
	return nil, errors.New("proc ownership unavailable")
}
