package mcp

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"

	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/reachability"
)

// ListenerProbe is the current L1 reachability mechanism. It lives with the
// responder so the request contract and its client stay in the same package;
// reachability selects it through the version-independent Probe interface.
type ListenerProbe struct {
	Client *http.Client
}

var _ reachability.Probe = (*ListenerProbe)(nil)

func NewListenerProbe() *ListenerProbe { return &ListenerProbe{} }

func (p *ListenerProbe) Probe(ctx context.Context, target reachability.Target) (bool, error) {
	if target.Port < 1 || target.Port > 65535 {
		return false, fmt.Errorf("invalid listener port %d", target.Port)
	}
	client := p.Client
	if client == nil {
		client = http.DefaultClient
	}
	url := "http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(target.Port)) + "/mcp"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false, fmt.Errorf("create listener probe request: %w", err)
	}
	req.Header.Set(ListenerProbeHeader, ListenerProbeHeaderValue)
	resp, err := client.Do(req)
	if err != nil {
		return false, fmt.Errorf("listener probe request: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("listener probe returned HTTP %d", resp.StatusCode)
	}
	return true, nil
}
