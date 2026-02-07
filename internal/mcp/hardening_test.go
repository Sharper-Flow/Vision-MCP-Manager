package mcp

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// --- MaxBytesMiddleware Tests ---

func TestMaxBytesMiddleware_UnderLimit(t *testing.T) {
	handler := MaxBytesMiddleware(1024)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write(body)
	}))

	payload := strings.Repeat("a", 512)
	req := httptest.NewRequest("POST", "/mcp", strings.NewReader(payload))
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200 for body under limit, got %d", w.Code)
	}
	if w.Body.String() != payload {
		t.Errorf("expected body to be echoed back")
	}
}

func TestMaxBytesMiddleware_OverLimit(t *testing.T) {
	handler := MaxBytesMiddleware(1024)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := io.ReadAll(r.Body)
		if err != nil {
			// MaxBytesReader triggers this error; the response is auto-set to 413
			// by http.MaxBytesReader when the handler tries to read beyond the limit.
			http.Error(w, "Request Entity Too Large", http.StatusRequestEntityTooLarge)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))

	payload := strings.Repeat("a", 2048) // 2x the limit
	req := httptest.NewRequest("POST", "/mcp", strings.NewReader(payload))
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("expected 413 for oversized body, got %d", w.Code)
	}
}

func TestMaxBytesMiddleware_ExactLimit(t *testing.T) {
	handler := MaxBytesMiddleware(1024)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "Request Entity Too Large", http.StatusRequestEntityTooLarge)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))

	payload := strings.Repeat("a", 1024) // exactly at limit
	req := httptest.NewRequest("POST", "/mcp", strings.NewReader(payload))
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200 for body at exact limit, got %d", w.Code)
	}
}

func TestMaxBytesMiddleware_EmptyBody(t *testing.T) {
	handler := MaxBytesMiddleware(1024)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "Request Entity Too Large", http.StatusRequestEntityTooLarge)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/mcp", nil)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200 for empty body, got %d", w.Code)
	}
}

// --- Rate Limiter Tests ---

func TestRateLimitMiddleware_AllowsBurst(t *testing.T) {
	handler := RateLimitMiddleware(5, time.Hour)(dummyHandler) // 5 burst, very slow refill

	for i := 0; i < 5; i++ {
		req := httptest.NewRequest("POST", "/mcp", nil)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("request %d: expected 200, got %d", i, w.Code)
		}
	}
}

func TestRateLimitMiddleware_RejectsOverBurst(t *testing.T) {
	handler := RateLimitMiddleware(3, time.Hour)(dummyHandler) // 3 burst, very slow refill

	// Exhaust burst
	for i := 0; i < 3; i++ {
		req := httptest.NewRequest("POST", "/mcp", nil)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
	}

	// Next request should be rate-limited
	req := httptest.NewRequest("POST", "/mcp", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusTooManyRequests {
		t.Errorf("expected 429 after burst exhausted, got %d", w.Code)
	}

	// Verify Retry-After header
	if got := w.Header().Get("Retry-After"); got != "1" {
		t.Errorf("expected Retry-After=1, got %q", got)
	}
}

func TestRateLimitMiddleware_RefillsOverTime(t *testing.T) {
	handler := RateLimitMiddleware(1, 10*time.Millisecond)(dummyHandler)

	// Use the one token
	req := httptest.NewRequest("POST", "/mcp", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("first request: expected 200, got %d", w.Code)
	}

	// Should be rate limited now
	req2 := httptest.NewRequest("POST", "/mcp", nil)
	w2 := httptest.NewRecorder()
	handler.ServeHTTP(w2, req2)

	if w2.Code != http.StatusTooManyRequests {
		t.Fatalf("second request: expected 429, got %d", w2.Code)
	}

	// Wait for refill
	time.Sleep(30 * time.Millisecond)

	// Should be allowed again
	req3 := httptest.NewRequest("POST", "/mcp", nil)
	w3 := httptest.NewRecorder()
	handler.ServeHTTP(w3, req3)

	if w3.Code != http.StatusOK {
		t.Errorf("third request after refill: expected 200, got %d", w3.Code)
	}
}

// --- HardeningConfig Tests ---

func TestHardeningConfig_Defaults(t *testing.T) {
	cfg := HardeningConfig{}

	if got := cfg.resolvedMaxBodyBytes(); got != DefaultMaxBodyBytes {
		t.Errorf("default max body bytes: expected %d, got %d", DefaultMaxBodyBytes, got)
	}
	if got := cfg.resolvedReadTimeout(); got != DefaultReadTimeout {
		t.Errorf("default read timeout: expected %v, got %v", DefaultReadTimeout, got)
	}
	if got := cfg.resolvedReadHeaderTimeout(); got != DefaultReadHeaderTimeout {
		t.Errorf("default read header timeout: expected %v, got %v", DefaultReadHeaderTimeout, got)
	}
	if got := cfg.resolvedWriteTimeout(); got != 0 {
		t.Errorf("default write timeout: expected 0 (SSE-safe), got %v", got)
	}
	if got := cfg.resolvedIdleTimeout(); got != DefaultIdleTimeout {
		t.Errorf("default idle timeout: expected %v, got %v", DefaultIdleTimeout, got)
	}
	if got := cfg.resolvedRateBurst(); got != DefaultRateBurst {
		t.Errorf("default rate burst: expected %d, got %d", DefaultRateBurst, got)
	}
	if got := cfg.resolvedRateInterval(); got != DefaultRateInterval {
		t.Errorf("default rate interval: expected %v, got %v", DefaultRateInterval, got)
	}
}

func TestHardeningConfig_CustomValues(t *testing.T) {
	cfg := HardeningConfig{
		MaxBodyBytes:      8 * 1024 * 1024,
		ReadTimeout:       60 * time.Second,
		ReadHeaderTimeout: 20 * time.Second,
		WriteTimeout:      45 * time.Second,
		IdleTimeout:       300 * time.Second,
		RateBurst:         100,
		RateInterval:      100 * time.Millisecond,
	}

	if got := cfg.resolvedMaxBodyBytes(); got != 8*1024*1024 {
		t.Errorf("custom max body bytes: expected %d, got %d", 8*1024*1024, got)
	}
	if got := cfg.resolvedReadTimeout(); got != 60*time.Second {
		t.Errorf("custom read timeout: expected 60s, got %v", got)
	}
	if got := cfg.resolvedReadHeaderTimeout(); got != 20*time.Second {
		t.Errorf("custom read header timeout: expected 20s, got %v", got)
	}
	if got := cfg.resolvedWriteTimeout(); got != 45*time.Second {
		t.Errorf("custom write timeout: expected 45s, got %v", got)
	}
	if got := cfg.resolvedIdleTimeout(); got != 300*time.Second {
		t.Errorf("custom idle timeout: expected 300s, got %v", got)
	}
	if got := cfg.resolvedRateBurst(); got != 100 {
		t.Errorf("custom rate burst: expected 100, got %d", got)
	}
	if got := cfg.resolvedRateInterval(); got != 100*time.Millisecond {
		t.Errorf("custom rate interval: expected 100ms, got %v", got)
	}
}

func TestHardeningConfig_RateBurstDisabled(t *testing.T) {
	cfg := HardeningConfig{RateBurst: -1}
	if got := cfg.resolvedRateBurst(); got != -1 {
		t.Errorf("disabled rate burst: expected -1, got %d", got)
	}
}

func TestHardeningConfig_ApplyServerTimeouts(t *testing.T) {
	cfg := HardeningConfig{
		ReadTimeout:       15 * time.Second,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	srv := &http.Server{}
	cfg.ApplyServerTimeouts(srv)

	if srv.ReadTimeout != 15*time.Second {
		t.Errorf("expected ReadTimeout=15s, got %v", srv.ReadTimeout)
	}
	if srv.ReadHeaderTimeout != 5*time.Second {
		t.Errorf("expected ReadHeaderTimeout=5s, got %v", srv.ReadHeaderTimeout)
	}
	if srv.WriteTimeout != 0 {
		t.Errorf("expected WriteTimeout=0 (SSE-safe), got %v", srv.WriteTimeout)
	}
	if srv.IdleTimeout != 60*time.Second {
		t.Errorf("expected IdleTimeout=60s, got %v", srv.IdleTimeout)
	}
}

// --- Integration: AddStreamable with Hardening ---

func TestAddStreamable_AppliesHardening(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	pm := NewPortManager(logger)
	defer pm.Close()

	handler := &noopHandler{}
	if err := pm.AddStreamable("hardened-server", 16290, handler, nil); err != nil {
		t.Fatalf("failed to add server: %v", err)
	}

	listener := pm.Get("hardened-server")
	if listener == nil {
		t.Fatal("listener not found")
	}

	srv := listener.Server

	// Verify timeouts are set (using defaults)
	if srv.ReadTimeout != DefaultReadTimeout {
		t.Errorf("expected ReadTimeout=%v, got %v", DefaultReadTimeout, srv.ReadTimeout)
	}
	if srv.ReadHeaderTimeout != DefaultReadHeaderTimeout {
		t.Errorf("expected ReadHeaderTimeout=%v, got %v", DefaultReadHeaderTimeout, srv.ReadHeaderTimeout)
	}
	if srv.WriteTimeout != 0 {
		t.Errorf("expected WriteTimeout=0 (SSE-safe), got %v", srv.WriteTimeout)
	}
	if srv.IdleTimeout != DefaultIdleTimeout {
		t.Errorf("expected IdleTimeout=%v, got %v", DefaultIdleTimeout, srv.IdleTimeout)
	}
}

func TestAddStreamable_BodySizeLimit(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	pm := NewPortManager(logger)
	defer pm.Close()

	// Echo handler that reads body
	echoHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "Request Entity Too Large", http.StatusRequestEntityTooLarge)
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	port := 16291
	if err := pm.AddStreamable("body-limit-test", port, echoHandler, nil); err != nil {
		t.Fatalf("failed to add server: %v", err)
	}

	// Wait for listener to be ready
	time.Sleep(50 * time.Millisecond)

	// Request with body under limit should succeed
	smallBody := bytes.NewReader(make([]byte, 1024))
	resp, err := http.Post("http://127.0.0.1:16291/mcp", "application/json", smallBody)
	if err != nil {
		t.Fatalf("small body request failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("small body: expected 200, got %d", resp.StatusCode)
	}

	// Request with body over limit (5 MB > 4 MB default) should fail
	largeBody := bytes.NewReader(make([]byte, 5*1024*1024))
	resp2, err := http.Post("http://127.0.0.1:16291/mcp", "application/json", largeBody)
	if err != nil {
		t.Fatalf("large body request failed: %v", err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("large body: expected 413, got %d", resp2.StatusCode)
	}
}

func TestAddStreamable_RateLimit(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	pm := NewPortManager(logger)
	defer pm.Close()

	handler := &noopHandler{}

	// Use custom hardening with very low burst for testing
	port := 16292
	if err := pm.AddStreamableWithHardening("rate-limit-test", port, handler, nil, HardeningConfig{
		RateBurst:    3,
		RateInterval: time.Hour, // no refill during test
	}); err != nil {
		t.Fatalf("failed to add server: %v", err)
	}

	// Wait for listener to be ready
	time.Sleep(50 * time.Millisecond)

	// First 3 requests should succeed (burst)
	for i := 0; i < 3; i++ {
		resp, err := http.Post("http://127.0.0.1:16292/mcp", "application/json", nil)
		if err != nil {
			t.Fatalf("request %d failed: %v", i, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("request %d: expected 200, got %d", i, resp.StatusCode)
		}
	}

	// 4th request should be rate limited
	resp, err := http.Post("http://127.0.0.1:16292/mcp", "application/json", nil)
	if err != nil {
		t.Fatalf("rate-limited request failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("expected 429 after burst exhausted, got %d", resp.StatusCode)
	}
}

// --- Token Bucket Unit Tests ---

func TestTokenBucket_BasicFlow(t *testing.T) {
	tb := newTokenBucket(3, time.Hour)

	// Should allow 3
	for i := 0; i < 3; i++ {
		if !tb.allow() {
			t.Errorf("request %d should be allowed", i)
		}
	}

	// 4th should be denied
	if tb.allow() {
		t.Error("4th request should be denied")
	}
}

func TestTokenBucket_Refill(t *testing.T) {
	tb := newTokenBucket(1, 10*time.Millisecond)

	if !tb.allow() {
		t.Fatal("first request should be allowed")
	}
	if tb.allow() {
		t.Fatal("second request should be denied")
	}

	time.Sleep(30 * time.Millisecond)

	if !tb.allow() {
		t.Error("request after refill should be allowed")
	}
}

func TestTokenBucket_NeverExceedsBurst(t *testing.T) {
	tb := newTokenBucket(2, time.Millisecond)

	// Let it refill well past burst
	time.Sleep(50 * time.Millisecond)

	// Should allow exactly burst count
	allowed := 0
	for i := 0; i < 10; i++ {
		if tb.allow() {
			allowed++
		}
	}

	if allowed != 2 {
		t.Errorf("expected exactly 2 allowed (burst cap), got %d", allowed)
	}
}
