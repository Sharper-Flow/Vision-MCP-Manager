//go:build !aix && !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris

package ownership

import "os"

func ownerMatches(os.FileInfo) bool { return false }
