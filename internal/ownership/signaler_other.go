//go:build !linux

package ownership

import "os"

type processGroupSignaler struct{}

func NewSignaler() Signaler                                   { return processGroupSignaler{} }
func (processGroupSignaler) Supported() bool                  { return false }
func (processGroupSignaler) GroupSignal(int, os.Signal) error { return os.ErrPermission }
