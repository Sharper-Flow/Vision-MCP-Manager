package mcp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/reachability"
)

const endToEndCleanupTimeout = 2 * time.Second

// EndToEndProbe exercises the public MCP endpoint with the smallest portable
// client flow: initialize, then delete the session it created. Unlike the
// listener probe, this reaches the backend and exercises admission and session
// lifecycle handling.
type EndToEndProbe struct {
	Client *http.Client
}

var _ reachability.Probe = (*EndToEndProbe)(nil)

func NewEndToEndProbe() *EndToEndProbe { return &EndToEndProbe{} }

func (p *EndToEndProbe) Probe(ctx context.Context, target reachability.Target) (reachable bool, err error) {
	if target.Port < 1 || target.Port > 65535 {
		return false, fmt.Errorf("invalid listener port %d", target.Port)
	}
	client := p.Client
	if client == nil {
		client = http.DefaultClient
	}
	endpoint := "http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(target.Port)) + "/mcp"
	sessionID := ""
	defer func() {
		if sessionID == "" {
			return
		}
		cleanupErr := deleteProbeSession(client, endpoint, target.BearerToken, sessionID)
		if cleanupErr == nil {
			return
		}
		reachable = false
		if err == nil {
			err = cleanupErr
			return
		}
		err = errors.Join(err, cleanupErr)
	}()

	initialize, requestErr := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(managedProbeInitialize))
	if requestErr != nil {
		return false, fmt.Errorf("create end-to-end probe request: %w", requestErr)
	}
	initialize.Header.Set("Content-Type", "application/json")
	initialize.Header.Set("Accept", "application/json, text/event-stream")
	initialize.Header.Set("Mcp-Protocol-Version", "2025-03-26")
	if target.BearerToken != "" {
		initialize.Header.Set("Authorization", "Bearer "+target.BearerToken)
	}
	resp, requestErr := client.Do(initialize)
	if resp != nil {
		// Capture the session before interpreting either the HTTP status or a
		// client error. A redirect/transport error can still leave a session
		// created upstream, so the defensive DELETE must not be skipped.
		sessionID = strings.TrimSpace(resp.Header.Get("Mcp-Session-Id"))
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
	}
	if requestErr != nil {
		return false, fmt.Errorf("end-to-end initialize: %w", requestErr)
	}
	if resp == nil {
		return false, errors.New("end-to-end initialize returned no response")
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		// A full admission gate answered the request. That proves the listener
		// and gateway are reachable; denial consumed no new reservation.
		return true, nil
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return false, fmt.Errorf("end-to-end initialize returned HTTP %d", resp.StatusCode)
	}
	if sessionID == "" {
		return false, errors.New("end-to-end initialize missing Mcp-Session-Id")
	}
	return true, nil
}

func deleteProbeSession(client *http.Client, endpoint, bearerToken, sessionID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), endToEndCleanupTimeout)
	defer cancel()
	cleanup, err := http.NewRequestWithContext(ctx, http.MethodDelete, endpoint, nil)
	if err != nil {
		return fmt.Errorf("create end-to-end cleanup request: %w", err)
	}
	cleanup.Header.Set("Mcp-Session-Id", sessionID)
	cleanup.Header.Set("Mcp-Protocol-Version", "2025-03-26")
	if bearerToken != "" {
		cleanup.Header.Set("Authorization", "Bearer "+bearerToken)
	}
	resp, err := client.Do(cleanup)
	if err != nil {
		return fmt.Errorf("end-to-end cleanup: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusNotFound && (resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices) {
		return fmt.Errorf("end-to-end cleanup returned HTTP %d", resp.StatusCode)
	}
	return nil
}
