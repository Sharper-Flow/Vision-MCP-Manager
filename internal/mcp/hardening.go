// hardening.go provides HTTP server hardening for MCP Streamable HTTP endpoints.
//
// Protections applied:
//   - Request body size limits (prevents DoS via large payloads)
//   - HTTP server timeouts (ReadTimeout, ReadHeaderTimeout, WriteTimeout, IdleTimeout)
//   - Per-listener rate limiting for /mcp endpoint (token-bucket)
package mcp

import (
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// --- Defaults ---

const (
	// DefaultMaxBodyBytes is the maximum request body size for MCP endpoints (4 MB).
	// MCP JSON-RPC messages are typically small (< 100 KB). 4 MB provides
	// headroom for tool arguments with embedded data while preventing abuse.
	DefaultMaxBodyBytes int64 = 4 * 1024 * 1024

	// DefaultReadTimeout is the max duration for reading the entire request (header + body).
	DefaultReadTimeout = 30 * time.Second

	// DefaultReadHeaderTimeout is the max duration for reading request headers.
	DefaultReadHeaderTimeout = 10 * time.Second

	// DefaultWriteTimeout is the max duration for writing the response.
	// Set to 0 (unlimited) because SSE streams must stay open indefinitely.
	DefaultWriteTimeout = 0

	// DefaultIdleTimeout is the max duration an idle keep-alive connection is kept open.
	DefaultIdleTimeout = 120 * time.Second

	// DefaultRateBurst is the max burst of requests allowed through the rate limiter.
	DefaultRateBurst = 50

	// DefaultRateInterval is the refill interval for the rate limiter bucket.
	// One token per interval, so 50 burst / 50ms refill = ~1000 req/s sustained.
	DefaultRateInterval = 50 * time.Millisecond
)

// HardeningConfig holds hardening settings. Zero values use defaults.
type HardeningConfig struct {
	// MaxBodyBytes caps the request body size. 0 uses DefaultMaxBodyBytes.
	MaxBodyBytes int64

	// ReadTimeout for http.Server. 0 uses DefaultReadTimeout.
	ReadTimeout time.Duration

	// ReadHeaderTimeout for http.Server. 0 uses DefaultReadHeaderTimeout.
	ReadHeaderTimeout time.Duration

	// WriteTimeout for http.Server. Negative disables (0 = SSE-safe default = no timeout).
	WriteTimeout time.Duration

	// IdleTimeout for http.Server. 0 uses DefaultIdleTimeout.
	IdleTimeout time.Duration

	// RateBurst is the token bucket burst size. 0 uses DefaultRateBurst.
	// Set to -1 to disable rate limiting.
	RateBurst int

	// RateInterval is the token refill interval. 0 uses DefaultRateInterval.
	RateInterval time.Duration
}

// resolvedMaxBodyBytes returns the effective max body bytes.
func (h HardeningConfig) resolvedMaxBodyBytes() int64 {
	if h.MaxBodyBytes > 0 {
		return h.MaxBodyBytes
	}
	return DefaultMaxBodyBytes
}

// resolvedReadTimeout returns the effective read timeout.
func (h HardeningConfig) resolvedReadTimeout() time.Duration {
	if h.ReadTimeout > 0 {
		return h.ReadTimeout
	}
	return DefaultReadTimeout
}

// resolvedReadHeaderTimeout returns the effective read header timeout.
func (h HardeningConfig) resolvedReadHeaderTimeout() time.Duration {
	if h.ReadHeaderTimeout > 0 {
		return h.ReadHeaderTimeout
	}
	return DefaultReadHeaderTimeout
}

// resolvedWriteTimeout returns the effective write timeout.
// Returns 0 (unlimited) by default for SSE compatibility.
func (h HardeningConfig) resolvedWriteTimeout() time.Duration {
	return h.WriteTimeout // 0 = unlimited (SSE-safe)
}

// resolvedIdleTimeout returns the effective idle timeout.
func (h HardeningConfig) resolvedIdleTimeout() time.Duration {
	if h.IdleTimeout > 0 {
		return h.IdleTimeout
	}
	return DefaultIdleTimeout
}

// resolvedRateBurst returns the effective rate burst.
func (h HardeningConfig) resolvedRateBurst() int {
	if h.RateBurst > 0 {
		return h.RateBurst
	}
	if h.RateBurst < 0 {
		return -1 // disabled
	}
	return DefaultRateBurst
}

// resolvedRateInterval returns the effective rate interval.
func (h HardeningConfig) resolvedRateInterval() time.Duration {
	if h.RateInterval > 0 {
		return h.RateInterval
	}
	return DefaultRateInterval
}

// ApplyServerTimeouts applies hardening timeouts to an http.Server.
func (h HardeningConfig) ApplyServerTimeouts(srv *http.Server) {
	srv.ReadTimeout = h.resolvedReadTimeout()
	srv.ReadHeaderTimeout = h.resolvedReadHeaderTimeout()
	srv.WriteTimeout = h.resolvedWriteTimeout()
	srv.IdleTimeout = h.resolvedIdleTimeout()
}

// --- Body Size Middleware ---

// MaxBytesMiddleware wraps a handler to enforce a request body size limit.
// Requests exceeding the limit receive 413 Request Entity Too Large.
func MaxBytesMiddleware(maxBytes int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
			next.ServeHTTP(w, r)
		})
	}
}

// --- Token Bucket Rate Limiter ---

// tokenBucket is a simple thread-safe token bucket rate limiter.
type tokenBucket struct {
	mu       sync.Mutex
	tokens   int
	burst    int
	interval time.Duration
	lastTime time.Time
}

// newTokenBucket creates a rate limiter with the given burst and refill interval.
func newTokenBucket(burst int, interval time.Duration) *tokenBucket {
	return &tokenBucket{
		tokens:   burst,
		burst:    burst,
		interval: interval,
		lastTime: time.Now(),
	}
}

// allow checks whether a request is allowed. Returns true if allowed.
func (tb *tokenBucket) allow() bool {
	tb.mu.Lock()
	defer tb.mu.Unlock()

	now := time.Now()
	elapsed := now.Sub(tb.lastTime)
	tb.lastTime = now

	// Refill tokens based on elapsed time.
	refill := int(elapsed / tb.interval)
	tb.tokens += refill
	if tb.tokens > tb.burst {
		tb.tokens = tb.burst
	}

	if tb.tokens > 0 {
		tb.tokens--
		return true
	}
	return false
}

// RateLimitMiddleware wraps a handler with a token-bucket rate limiter.
// Requests exceeding the rate receive 429 Too Many Requests.
// An optional logger can be provided to log rate limit denials.
func RateLimitMiddleware(burst int, interval time.Duration, logger ...*slog.Logger) func(http.Handler) http.Handler {
	bucket := newTokenBucket(burst, interval)
	var log *slog.Logger
	if len(logger) > 0 {
		log = logger[0]
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !bucket.allow() {
				if log != nil {
					log.Warn("rate limit exceeded",
						slog.String("event", "hardening.rate_limited"),
						slog.String("remote_addr", r.RemoteAddr),
						slog.String("method", r.Method),
						slog.String("path", r.URL.Path),
					)
				}
				w.Header().Set("Retry-After", "1")
				http.Error(w, "Too Many Requests", http.StatusTooManyRequests)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
