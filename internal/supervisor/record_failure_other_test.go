//go:build !windows && !linux

package supervisor

// expectedDelegatedGroupSignals returns the number of delegated signaler
// calls the production kill route makes on this platform. Ownership
// signaling is unsupported here (ownership.NewSignaler().Supported() is
// false), so forceKill bypasses the recording signaler and kills the process
// group through the platform helper in process_group_unix.go instead.
func expectedDelegatedGroupSignals() int { return 0 }
