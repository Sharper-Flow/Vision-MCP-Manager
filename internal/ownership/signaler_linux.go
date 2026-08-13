//go:build linux

package ownership

import (
	"os"
	"syscall"
)

type processGroupSignaler struct{}

func NewSignaler() Signaler                  { return processGroupSignaler{} }
func (processGroupSignaler) Supported() bool { return true }
func (processGroupSignaler) GroupSignal(pgid int, signal os.Signal) error {
	if pgid <= 0 {
		return syscall.EINVAL
	}
	sig, ok := signal.(syscall.Signal)
	if !ok {
		return syscall.EINVAL
	}
	return syscall.Kill(-pgid, sig)
}
