package mcp

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// dummyHandler returns 200 OK with "ok" body — used as the inner handler.
var dummyHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
})

func TestSecurityMiddleware_BearerAuth_ValidToken(t *testing.T) {
	cfg := SecurityConfig{
		BearerToken: "test-secret-token",
	}
	handler := SecurityMiddleware(cfg)(dummyHandler)

	req := httptest.NewRequest("POST", "/mcp", nil)
	req.Header.Set("Authorization", "Bearer test-secret-token")
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
}

func TestSecurityMiddleware_BearerAuth_MissingToken(t *testing.T) {
	cfg := SecurityConfig{
		BearerToken: "test-secret-token",
	}
	handler := SecurityMiddleware(cfg)(dummyHandler)

	req := httptest.NewRequest("POST", "/mcp", nil)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
}

func TestSecurityMiddleware_BearerAuth_WrongToken(t *testing.T) {
	cfg := SecurityConfig{
		BearerToken: "test-secret-token",
	}
	handler := SecurityMiddleware(cfg)(dummyHandler)

	req := httptest.NewRequest("POST", "/mcp", nil)
	req.Header.Set("Authorization", "Bearer wrong-token")
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
}

func TestSecurityMiddleware_BearerAuth_NotConfigured(t *testing.T) {
	// When no bearer token is configured, auth is not enforced (open access).
	cfg := SecurityConfig{}
	handler := SecurityMiddleware(cfg)(dummyHandler)

	req := httptest.NewRequest("POST", "/mcp", nil)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200 (no auth required), got %d", w.Code)
	}
}

func TestSecurityMiddleware_Origin_AllowedOrigin(t *testing.T) {
	cfg := SecurityConfig{
		AllowedOrigins: []string{"http://localhost:3000", "http://localhost:8080"},
	}
	handler := SecurityMiddleware(cfg)(dummyHandler)

	req := httptest.NewRequest("POST", "/mcp", nil)
	req.Header.Set("Origin", "http://localhost:3000")
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}

	// Should set CORS header for the specific origin
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "http://localhost:3000" {
		t.Errorf("expected ACAO=http://localhost:3000, got %q", got)
	}
	if got := w.Header().Get("Vary"); got != "Origin" {
		t.Errorf("expected Vary=Origin, got %q", got)
	}
}

func TestSecurityMiddleware_Origin_DisallowedOrigin(t *testing.T) {
	cfg := SecurityConfig{
		AllowedOrigins: []string{"http://localhost:3000"},
	}
	handler := SecurityMiddleware(cfg)(dummyHandler)

	req := httptest.NewRequest("POST", "/mcp", nil)
	req.Header.Set("Origin", "http://evil.com")
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403, got %d", w.Code)
	}
}

func TestSecurityMiddleware_Origin_WildcardRejected(t *testing.T) {
	// Explicitly configuring "*" as an allowed origin should be rejected.
	cfg := SecurityConfig{
		AllowedOrigins: []string{"*"},
	}
	handler := SecurityMiddleware(cfg)(dummyHandler)

	req := httptest.NewRequest("POST", "/mcp", nil)
	req.Header.Set("Origin", "http://any.com")
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403 (wildcard CORS rejected), got %d", w.Code)
	}
}

func TestSecurityMiddleware_Origin_NoOriginHeader(t *testing.T) {
	// Requests without Origin header (e.g., same-origin, curl) should be allowed
	// when origin allowlist is configured.
	cfg := SecurityConfig{
		AllowedOrigins: []string{"http://localhost:3000"},
	}
	handler := SecurityMiddleware(cfg)(dummyHandler)

	req := httptest.NewRequest("POST", "/mcp", nil)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200 (no origin header = same-origin), got %d", w.Code)
	}
}

func TestSecurityMiddleware_Origin_NotConfigured(t *testing.T) {
	// When no origins are configured, origin checking is not enforced.
	cfg := SecurityConfig{}
	handler := SecurityMiddleware(cfg)(dummyHandler)

	req := httptest.NewRequest("POST", "/mcp", nil)
	req.Header.Set("Origin", "http://any.com")
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200 (no origin policy), got %d", w.Code)
	}
}

func TestSecurityMiddleware_CORS_Preflight(t *testing.T) {
	cfg := SecurityConfig{
		BearerToken:    "secret",
		AllowedOrigins: []string{"http://localhost:3000"},
	}
	handler := SecurityMiddleware(cfg)(dummyHandler)

	req := httptest.NewRequest("OPTIONS", "/mcp", nil)
	req.Header.Set("Origin", "http://localhost:3000")
	req.Header.Set("Access-Control-Request-Method", "POST")
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	// Preflight should NOT require bearer auth
	if w.Code != http.StatusNoContent {
		t.Errorf("expected 204, got %d", w.Code)
	}

	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "http://localhost:3000" {
		t.Errorf("expected ACAO=http://localhost:3000, got %q", got)
	}
	if got := w.Header().Get("Access-Control-Allow-Methods"); got == "" {
		t.Error("expected Access-Control-Allow-Methods header")
	}
	if got := w.Header().Get("Access-Control-Allow-Headers"); got == "" {
		t.Error("expected Access-Control-Allow-Headers header")
	}
}

func TestSecurityMiddleware_CORS_PreflightDisallowedOrigin(t *testing.T) {
	cfg := SecurityConfig{
		AllowedOrigins: []string{"http://localhost:3000"},
	}
	handler := SecurityMiddleware(cfg)(dummyHandler)

	req := httptest.NewRequest("OPTIONS", "/mcp", nil)
	req.Header.Set("Origin", "http://evil.com")
	req.Header.Set("Access-Control-Request-Method", "POST")
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403 for disallowed origin preflight, got %d", w.Code)
	}
}

func TestSecurityMiddleware_Combined_AuthAndOrigin(t *testing.T) {
	cfg := SecurityConfig{
		BearerToken:    "secret",
		AllowedOrigins: []string{"http://localhost:3000"},
	}
	handler := SecurityMiddleware(cfg)(dummyHandler)

	// Both valid token and valid origin
	req := httptest.NewRequest("POST", "/mcp", nil)
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Origin", "http://localhost:3000")
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}

	// Valid token but wrong origin
	req2 := httptest.NewRequest("POST", "/mcp", nil)
	req2.Header.Set("Authorization", "Bearer secret")
	req2.Header.Set("Origin", "http://evil.com")
	w2 := httptest.NewRecorder()

	handler.ServeHTTP(w2, req2)

	if w2.Code != http.StatusForbidden {
		t.Errorf("expected 403 for wrong origin, got %d", w2.Code)
	}

	// Valid origin but wrong token
	req3 := httptest.NewRequest("POST", "/mcp", nil)
	req3.Header.Set("Authorization", "Bearer wrong")
	req3.Header.Set("Origin", "http://localhost:3000")
	w3 := httptest.NewRecorder()

	handler.ServeHTTP(w3, req3)

	if w3.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for wrong token, got %d", w3.Code)
	}
}

func TestSecurityMiddleware_NoCORSWildcard_InResponse(t *testing.T) {
	// Even when origin checking is disabled, the middleware should never set
	// Access-Control-Allow-Origin: * in the response.
	cfg := SecurityConfig{}
	handler := SecurityMiddleware(cfg)(dummyHandler)

	req := httptest.NewRequest("POST", "/mcp", nil)
	req.Header.Set("Origin", "http://any.com")
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if got := w.Header().Get("Access-Control-Allow-Origin"); got == "*" {
		t.Error("must never set Access-Control-Allow-Origin: * on MCP endpoints")
	}
}
