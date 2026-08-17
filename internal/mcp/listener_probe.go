package mcp

import (
	"io"
	"net/http"
)

const (
	// ListenerProbeHeader identifies the internal listener liveness probe.
	// The probe driver must send this header with ListenerProbeHeaderValue.
	ListenerProbeHeader = "X-Vision-MCP-Probe"
	// ListenerProbeHeaderValue distinguishes the internal listener probe from
	// normal MCP client traffic.
	ListenerProbeHeaderValue = "listener-v1"
	// ListenerProbeResponse is the complete, fixed response body for a listener
	// probe. It intentionally contains no request or server information.
	ListenerProbeResponse = "ok\n"
)

// ListenerProbeMiddleware answers the internal listener liveness probe without
// invoking the wrapped handler. It is intended to be the outermost middleware
// so probes do not consume rate-limit budget or require bearer authentication.
func ListenerProbeMiddleware() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet && r.URL.Path == "/mcp" && r.Header.Get(ListenerProbeHeader) == ListenerProbeHeaderValue {
				w.Header().Set("Content-Type", "text/plain; charset=utf-8")
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, ListenerProbeResponse)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}
