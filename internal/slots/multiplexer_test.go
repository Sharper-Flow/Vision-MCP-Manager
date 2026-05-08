package slots

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/config"
	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/session"
)

func TestSelectForNewSession_LeastLoadedTieBreaksByIndex(t *testing.T) {
	manager2 := session.NewManager("playwright-slot-2", &config.ServerConfig{Command: "echo"}, nil)
	manager1 := session.NewManager("playwright-slot-1", &config.ServerConfig{Command: "echo"}, nil)
	mx := NewMultiplexer("playwright", nil, []Entry{
		{SlotName: "playwright-slot-2", Index: 2, Manager: manager2},
		{SlotName: "playwright-slot-1", Index: 1, Manager: manager1},
	})

	first, err := mx.SelectForNewSession(context.Background(), "upstream-1")
	if err != nil {
		t.Fatalf("SelectForNewSession() first error: %v", err)
	}
	if first != manager1 {
		t.Fatalf("first manager mismatch; want slot index 1")
	}

	second, err := mx.SelectForNewSession(context.Background(), "upstream-2")
	if err != nil {
		t.Fatalf("SelectForNewSession() second error: %v", err)
	}
	if second != manager2 {
		t.Fatalf("second manager mismatch; want slot index 2 after pending load on slot 1")
	}
}

func TestSelectForNewSession_ReusesExistingBinding(t *testing.T) {
	mx := NewMultiplexer("playwright", nil, []Entry{{SlotName: "playwright-slot-1", Index: 1, Manager: session.NewManager("playwright-slot-1", &config.ServerConfig{Command: "echo"}, nil)}})

	first, err := mx.SelectForNewSession(context.Background(), "same-upstream")
	if err != nil {
		t.Fatalf("SelectForNewSession() first error: %v", err)
	}
	second, err := mx.SelectForNewSession(context.Background(), "same-upstream")
	if err != nil {
		t.Fatalf("SelectForNewSession() second error: %v", err)
	}
	if first != second {
		t.Fatalf("expected same manager for repeated upstream binding")
	}
}

func TestRelease_IsIdempotent(t *testing.T) {
	mx := NewMultiplexer("playwright", nil, []Entry{{SlotName: "playwright-slot-1", Index: 1, Manager: session.NewManager("playwright-slot-1", &config.ServerConfig{Command: "echo"}, nil)}})

	if _, err := mx.SelectForNewSession(context.Background(), "upstream-1"); err != nil {
		t.Fatalf("SelectForNewSession() error: %v", err)
	}

	mx.Release("upstream-1")
	mx.Release("upstream-1")

	if got := len(mx.byUpstream); got != 0 {
		t.Fatalf("len(byUpstream) = %d, want 0", got)
	}
}

func TestAdmissionStatus_AggregatesAcrossSlots(t *testing.T) {
	mx := NewMultiplexer("playwright", nil, []Entry{
		{SlotName: "playwright-slot-1", Index: 1, Manager: session.NewManager("playwright-slot-1", &config.ServerConfig{Command: "echo", MaxSessions: 2}, nil)},
		{SlotName: "playwright-slot-2", Index: 2, Manager: session.NewManager("playwright-slot-2", &config.ServerConfig{Command: "echo", MaxSessions: 3}, nil)},
	})

	_, _ = mx.SelectForNewSession(context.Background(), "upstream-1")
	_, _ = mx.SelectForNewSession(context.Background(), "upstream-2")

	atCapacity, current, max := mx.AdmissionStatus()
	if atCapacity {
		t.Fatal("atCapacity = true, want false")
	}
	if current != 2 {
		t.Fatalf("current = %d, want 2", current)
	}
	if max != 5 {
		t.Fatalf("max = %d, want 5", max)
	}
}

func TestAdmissionStatus_UnlimitedSlotPreventsCapacity(t *testing.T) {
	mx := NewMultiplexer("playwright", nil, []Entry{
		{SlotName: "bounded", Index: 1, Manager: session.NewManager("bounded", &config.ServerConfig{Command: "echo", MaxSessions: 1}, nil)},
		{SlotName: "unlimited", Index: 2, Manager: session.NewManager("unlimited", &config.ServerConfig{Command: "echo", MaxSessions: 0}, nil)},
	})

	_, _ = mx.SelectForNewSession(context.Background(), "upstream-1")

	atCapacity, _, max := mx.AdmissionStatus()
	if atCapacity {
		t.Fatal("atCapacity = true, want false with unlimited slot")
	}
	if max != 0 {
		t.Fatalf("max = %d, want 0 when any slot is unlimited", max)
	}
}

func TestSelectAndRelease_EmitStructuredEvents(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	mx := NewMultiplexer("playwright", logger, []Entry{{SlotName: "playwright-slot-1", Index: 1, Manager: session.NewManager("playwright-slot-1", &config.ServerConfig{Command: "echo"}, nil)}})

	if _, err := mx.SelectForNewSession(context.Background(), "upstream-1"); err != nil {
		t.Fatalf("SelectForNewSession() error: %v", err)
	}
	mx.Release("upstream-1")

	logs := buf.String()
	if !strings.Contains(logs, "event=slot.assigned") {
		t.Fatalf("logs missing slot.assigned event: %s", logs)
	}
	if !strings.Contains(logs, "event=slot.released") {
		t.Fatalf("logs missing slot.released event: %s", logs)
	}
	if !strings.Contains(logs, "upstream_session_id=upstream-1") {
		t.Fatalf("logs missing upstream_session_id: %s", logs)
	}
}

func TestSelectForNewSession_AllSlotsFull_EmitsRoutingFull(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	mx := NewMultiplexer("playwright", logger, []Entry{{SlotName: "bounded", Index: 1, Manager: session.NewManager("bounded", &config.ServerConfig{Command: "echo", MaxSessions: 1}, nil)}})

	if _, err := mx.SelectForNewSession(context.Background(), "upstream-1"); err != nil {
		t.Fatalf("first SelectForNewSession() error: %v", err)
	}
	if _, err := mx.SelectForNewSession(context.Background(), "upstream-2"); err == nil {
		t.Fatal("second SelectForNewSession() expected full-capacity error, got nil")
	}

	logs := buf.String()
	if !strings.Contains(logs, "event=slot.routing_full") {
		t.Fatalf("logs missing slot.routing_full event: %s", logs)
	}
}

func TestSelectForNewSession_SkipsRegistryUnhealthySlots(t *testing.T) {
	manager1 := session.NewManager("playwright-slot-1", &config.ServerConfig{Command: "echo"}, nil)
	manager2 := session.NewManager("playwright-slot-2", &config.ServerConfig{Command: "echo"}, nil)
	mx := NewMultiplexer("playwright", nil, []Entry{
		{SlotName: "playwright-slot-1", Index: 1, Manager: manager1, Healthy: func() bool { return false }},
		{SlotName: "playwright-slot-2", Index: 2, Manager: manager2, Healthy: func() bool { return true }},
	})

	selected, err := mx.SelectForNewSession(context.Background(), "upstream-1")
	if err != nil {
		t.Fatalf("SelectForNewSession() error: %v", err)
	}
	if selected != manager2 {
		t.Fatalf("selected wrong manager; unhealthy slot should be skipped")
	}
}

func TestReportSpawnResult_FailedSlotGetsSkippedTemporarily(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	manager1 := session.NewManager("playwright-slot-1", &config.ServerConfig{Command: "echo"}, nil)
	manager2 := session.NewManager("playwright-slot-2", &config.ServerConfig{Command: "echo"}, nil)
	mx := NewMultiplexer("playwright", logger, []Entry{
		{SlotName: "playwright-slot-1", Index: 1, Manager: manager1, Healthy: func() bool { return true }},
		{SlotName: "playwright-slot-2", Index: 2, Manager: manager2, Healthy: func() bool { return true }},
	})

	selected, err := mx.SelectForNewSession(context.Background(), "upstream-1")
	if err != nil {
		t.Fatalf("SelectForNewSession() error: %v", err)
	}
	if selected != manager1 {
		t.Fatalf("expected slot 1 on first selection")
	}
	mx.ReportSpawnResult("upstream-1", assertErr("boom"))
	mx.Release("upstream-1")

	selected, err = mx.SelectForNewSession(context.Background(), "upstream-2")
	if err != nil {
		t.Fatalf("second SelectForNewSession() error: %v", err)
	}
	if selected != manager2 {
		t.Fatalf("expected failed slot to be skipped on next selection")
	}
	if !strings.Contains(buf.String(), "event=slot.health_skipped") {
		t.Fatalf("logs missing slot.health_skipped event: %s", buf.String())
	}
}

func assertErr(msg string) error { return errors.New(msg) }
