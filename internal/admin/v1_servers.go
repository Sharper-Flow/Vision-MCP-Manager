package admin

import (
	"encoding/json"
	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/server"
	"net/http"
)

// handleV1Servers handles GET /v1/servers — returns a JSON envelope
// {"servers": [ServerStatus, ...]} built from the registry snapshot.
//
// Public endpoint (no SecurityMiddleware). Mirrors /health in that the
// admin server is localhost-bound by default and status visibility is
// considered low-risk. last_error strings are routed through scrubSecrets
// before serialization to prevent token/key leakage (see V1 refinement
// task tk-tIycaEpQ).
func (s *Server) handleV1Servers(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	running := s.running
	s.mu.RUnlock()

	if !running {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "unhealthy"})
		return
	}

	statuses := make([]map[string]any, 0)
	if s.registry != nil {
		for _, srv := range s.registry.List() {
			st := srv.Status()
			entry := s.v1ServerEntry(st)
			statuses = append(statuses, entry)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"servers": statuses})
}

// handleV1ServerDetail handles GET /v1/servers/{name} — returns a single
// server's status, or 404 if the name is not registered.
func (s *Server) handleV1ServerDetail(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		http.Error(w, "missing server name", http.StatusBadRequest)
		return
	}

	s.mu.RLock()
	running := s.running
	s.mu.RUnlock()
	if !running {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "unhealthy"})
		return
	}

	if s.registry == nil {
		http.Error(w, "registry unavailable", http.StatusServiceUnavailable)
		return
	}

	for _, srv := range s.registry.List() {
		if srv.Name != name {
			continue
		}
		st := srv.Status()
		entry := s.v1ServerEntry(st)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(entry)
		return
	}

	http.Error(w, "server not found", http.StatusNotFound)
}

func (s *Server) v1ServerEntry(st server.ServerStatus) map[string]any {
	lifecycle := s.lifecycleSnapshot(st.Name)
	effective := deriveEffectiveStatus(st.State, lifecycleBackendState(lifecycle), s.reachabilityFor(st.Name), st.Uptime, s.reachabilityGrace, st.LastError)
	reachability := effective.Reachability
	entry := map[string]any{
		"name": st.Name, "port": st.Port, "transport": string(st.Transport),
		"state": string(st.State), "process_state": string(st.State),
		"effective_status": effective.Status, "autostart": st.Autostart,
		"required": st.Required, "pid": st.PID, "uptime_seconds": int64(st.Uptime.Seconds()),
		"restart_count": st.RestartCount, "last_error": scrubSecrets(st.LastError),
		"reachability":               string(reachability.Reachability),
		"probe_depth":                reachability.ProbeDepth,
		"last_probe_at":              reachability.LastProbeAt,
		"last_probe_outcome":         reachability.LastProbeOutcome,
		"last_probe_error":           reachability.LastProbeError,
		"consecutive_probe_failures": reachability.ConsecutiveProbeFailures,
	}
	if effective.Reason != "" {
		entry["effective_reason"] = effective.Reason
	}
	if s.serverMetricsAccessor != nil {
		if snap := s.serverMetricsAccessor.ServerMetricsSnapshot(st.Name); snap != nil {
			entry["session_metrics"] = snap
		}
	}
	if lifecycle != nil {
		entry["session_lifecycle"] = lifecycle
	}
	return entry
}

// registerV1Routes registers the V1 admin endpoints on the provided mux.
// Called from Start alongside the existing /health and /healthz handlers.
func (s *Server) registerV1Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/servers", s.handleV1Servers)
	mux.HandleFunc("GET /v1/servers/{name}", s.handleV1ServerDetail)
	mux.HandleFunc("GET /v1/slots", s.handleV1Slots)
	mux.HandleFunc("GET /v1/slots/{group}", s.handleV1SlotsGroup)
}

// scrubSecrets redacts common secret patterns from error messages before
// exposing them on public admin endpoints. Patterns covered:
//   - Authorization: Bearer <token>
//   - *_TOKEN / *_KEY / *_SECRET / *_PASSWORD = <value>
//   - password=<value>
//
// This is the V1 refinement tracked by task tk-tIycaEpQ. Tests live in
// v1_servers_scrub_test.go.
//
// scrubSecrets is intentionally conservative: on match, the value portion
// is replaced with ***REDACTED*** while the key name is preserved so
// operators can still identify which credential was referenced.
func scrubSecrets(in string) string {
	if in == "" {
		return in
	}
	return secretScrubber.scrub(in)
}
