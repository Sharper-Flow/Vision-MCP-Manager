//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package ownership

import (
	"os"
	"syscall"
)

func ownerMatches(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(stat.Uid) == os.Getuid()
}
