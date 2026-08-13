package mcp

// ListenerExposure classifies whether an HTTP listener is reachable beyond
// the local machine. It is structural metadata, not an address heuristic.
type ListenerExposure string

const (
	ListenerLoopback ListenerExposure = "loopback"
	ListenerNetwork  ListenerExposure = "network"
)

// BearerAuthWarning returns the only warning needed for an unauthenticated
// listener. The warning intentionally contains no token material.
func BearerAuthWarning(token string, exposure ListenerExposure) string {
	if token != "" || exposure != ListenerNetwork {
		return ""
	}
	return "⚠ No bearer_token configured on a network-exposed listener — see docs/AUTH.md for secure setup"
}
