package supervisor

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/config"
)

type lockedLogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestProxyStartLogOmitsURLCredentials covers remote URLs that carry a
// credential in the query or in userinfo. The log keeps the endpoint origin
// and path so the entry still identifies the remote.
func TestProxyStartLogOmitsURLCredentials(t *testing.T) {
	const secret = "vision-test-secret-3b29af57"
	cases := map[string]string{
		"query":    "https://remote.invalid/mcp/stream?userToken=" + secret,
		"userinfo": "https://user:" + secret + "@remote.invalid/mcp",
	}
	for name, rawURL := range cases {
		t.Run(name, func(t *testing.T) {
			logs := &lockedLogBuffer{}
			cfg := &config.ServerConfig{Port: 16290, Transport: config.TransportHTTP, URL: rawURL}
			proc := NewManagedProcess("remote-log", cfg, config.SupervisionConfig{},
				slog.New(slog.NewTextHandler(logs, nil)))

			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- proc.Serve(ctx) }()

			deadline := time.Now().Add(3 * time.Second)
			for !strings.Contains(logs.String(), "proxy server started") {
				if time.Now().After(deadline) {
					cancel()
					t.Fatalf("proxy start not logged; logs:\n%s", logs.String())
				}
				time.Sleep(10 * time.Millisecond)
			}
			cancel()
			<-done

			got := logs.String()
			if strings.Contains(got, secret) {
				t.Fatalf("proxy start log contains a URL credential:\n%s", got)
			}
			if !strings.Contains(got, "remote.invalid") {
				t.Fatalf("proxy start log lost the endpoint host:\n%s", got)
			}
		})
	}
}
