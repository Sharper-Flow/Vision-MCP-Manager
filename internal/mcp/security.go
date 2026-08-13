// security.go provides HTTP security middleware for MCP Streamable HTTP endpoints.
//
// The middleware enforces:
//   - Bearer token authentication (when configured)
//   - Strict Origin header allowlist (when configured)
//   - Rejection of wildcard CORS (Access-Control-Allow-Origin: * is never emitted)
//   - CORS preflight (OPTIONS) handling with proper headers
//
// Security checks run in order: Origin → Auth. CORS preflight (OPTIONS) skips
// auth but still validates Origin.
package mcp

import (
	"crypto/subtle"
	"log/slog"
	"net/http"
	"strings"
)

// SecurityConfig holds security settings for MCP endpoints.
// All fields are optional — when zero-valued, the corresponding check is skipped.
type SecurityConfig struct {
	// BearerToken is the expected bearer token for Authorization header validation.
	// When empty, authentication is not enforced (open access).
	BearerToken string

	// AllowedOrigins is the list of permitted Origin header values.
	// When empty, origin checking is not enforced.
	// Wildcard ("*") entries are explicitly rejected and treated as if origin is disallowed.
	AllowedOrigins []string

	// Logger for security events. When nil, security events are not logged.
	Logger *slog.Logger

	ListenerExposure ListenerExposure
}

// SecurityMiddleware returns an HTTP middleware that enforces bearer auth
// and Origin allowlist on MCP endpoints.
//
// Evaluation order:
//  1. Origin check (if AllowedOrigins configured and request has Origin header)
//  2. CORS preflight response (OPTIONS requests skip auth)
//  3. Bearer auth check (if BearerToken configured)
//  4. Pass to inner handler
func SecurityMiddleware(cfg SecurityConfig) func(http.Handler) http.Handler {
	// Pre-compute the allowed origins set for O(1) lookup.
	// Explicitly filter out "*" — wildcard CORS is never allowed.
	originsSet := make(map[string]struct{}, len(cfg.AllowedOrigins))
	originCheckEnabled := len(cfg.AllowedOrigins) > 0
	for _, o := range cfg.AllowedOrigins {
		if o != "*" {
			originsSet[o] = struct{}{}
		}
	}

	authEnabled := cfg.BearerToken != ""
	logger := cfg.Logger

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")

			// --- Origin check ---
			if originCheckEnabled && origin != "" {
				if _, allowed := originsSet[origin]; !allowed {
					if logger != nil {
						logger.Warn("origin rejected",
							slog.String("event", "security.origin_denied"),
							slog.String("origin", origin),
							slog.String("remote_addr", r.RemoteAddr),
							slog.String("method", r.Method),
							slog.String("path", r.URL.Path),
						)
					}
					http.Error(w, "Forbidden: origin not allowed", http.StatusForbidden)
					return
				}
				// Set CORS headers for the allowed origin (never wildcard).
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Set("Vary", "Origin")
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, Mcp-Session-Id, Mcp-Protocol-Version")
				w.Header().Set("Access-Control-Expose-Headers", "Mcp-Session-Id")
				w.Header().Set("Access-Control-Max-Age", "86400")
			}

			// --- CORS preflight ---
			// OPTIONS requests get CORS headers but skip auth.
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}

			// --- Bearer auth ---
			if authEnabled {
				authHeader := r.Header.Get("Authorization")
				if !validBearerToken(authHeader, cfg.BearerToken) {
					if logger != nil {
						reason := "missing"
						if authHeader != "" {
							reason = "invalid"
						}
						logger.Warn("auth rejected",
							slog.String("event", "security.auth_denied"),
							slog.String("reason", reason),
							slog.String("remote_addr", r.RemoteAddr),
							slog.String("method", r.Method),
							slog.String("path", r.URL.Path),
						)
					}
					http.Error(w, "Unauthorized: invalid or missing bearer token", http.StatusUnauthorized)
					return
				}
			}

			next.ServeHTTP(w, r)
		})
	}
}

// validBearerToken checks that the Authorization header contains the expected
// bearer token in the format "Bearer <token>".
func validBearerToken(header, expected string) bool {
	if header == "" || expected == "" {
		return false
	}
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) {
		return false
	}
	token := strings.TrimPrefix(header, prefix)
	return subtle.ConstantTimeCompare([]byte(token), []byte(expected)) == 1
}
