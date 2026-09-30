//go:build linux

package supervisor

// expectedDelegatedGroupSignals returns the number of delegated signaler
// calls the production kill route makes on this platform. On Linux the
// ownership signaler is supported, so forceKill delegates exactly one
// GroupSignal call to the real platform signaler.
func expectedDelegatedGroupSignals() int { return 1 }
