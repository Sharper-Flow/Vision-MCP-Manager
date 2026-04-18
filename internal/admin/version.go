package admin

import (
	"encoding/json"
	"net/http"
)

// Version and BuildInfo are populated by the cmd/vision entry point at
// process startup (usually via ldflags). Empty string is acceptable and
// will surface as "dev" in the /version response for local builds.
var (
	Version   = "dev"
	BuildInfo = ""
)

// apiCapabilities describes the admin HTTP surface shipped with this
// Vision binary. External tools (OpenCode Advance) probe /version to
// confirm the endpoints they rely on exist before making calls.
//
// Bump keys here when adding new /v1/* surfaces so clients can
// feature-detect without hard-pinning Vision versions.
var apiCapabilities = map[string]bool{
	"v1_servers":        true, // GET /v1/servers
	"v1_servers_detail": true, // GET /v1/servers/{name}
	"version":           true, // GET /version itself
}

// handleVersion serves GET /version — the admin version + capability
// contract. Public (no SecurityMiddleware). Mirrors /health in access
// model.
func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"version": Version,
		"build":   BuildInfo,
		"api":     apiCapabilities,
	})
}
