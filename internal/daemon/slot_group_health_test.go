package daemon

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/admin"
	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/config"
	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/reachability"
	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/server"
	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/session"
	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/slots"
)

func TestSlotServerHealthy_UsesReachabilityAndGrace(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name       string
		state      reachability.State
		startedAt  time.Time
		wantHealth bool
	}{
		{name: "unreachable", state: reachability.StateUnreachable, startedAt: now, wantHealth: false},
		{name: "reachable", state: reachability.StateReachable, startedAt: now, wantHealth: true},
		{name: "unprobed within grace", state: reachability.StateUnprobed, startedAt: now.Add(-2 * time.Second), wantHealth: true},
		{name: "probing", state: reachability.StateProbing, startedAt: now.Add(-2 * time.Minute), wantHealth: true},
		{name: "unprobed beyond grace", state: reachability.StateUnprobed, startedAt: now.Add(-2 * time.Minute), wantHealth: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := &server.ManagedServer{
				State:     server.StateRunning,
				StartedAt: tt.startedAt,
			}
			got := slotServerHealthy(srv, reachability.Reachability{State: tt.state}, admin.DefaultReachabilityGrace)
			if got != tt.wantHealth {
				t.Fatalf("slotServerHealthy() = %v, want %v", got, tt.wantHealth)
			}
		})
	}
}

func TestSlotGroup_AllServersUnreachableReturnsCapacityError(t *testing.T) {
	value := reachability.Reachability{State: reachability.StateUnreachable}
	srv := &server.ManagedServer{State: server.StateRunning, StartedAt: time.Now()}
	manager1 := session.NewManager("slot-1", &config.ServerConfig{Command: "echo"}, nil)
	manager2 := session.NewManager("slot-2", &config.ServerConfig{Command: "echo"}, nil)
	mx := slots.NewMultiplexer("group", nil, []slots.Entry{
		{SlotName: "slot-1", Index: 1, Manager: manager1, Healthy: func() bool {
			return slotServerHealthy(srv, value, admin.DefaultReachabilityGrace)
		}},
		{SlotName: "slot-2", Index: 2, Manager: manager2, Healthy: func() bool {
			return slotServerHealthy(srv, value, admin.DefaultReachabilityGrace)
		}},
	})

	selected, err := mx.SelectForNewSession(context.Background(), "upstream-1")
	if selected != nil {
		t.Fatalf("selected manager %v from unreachable slots", selected)
	}
	if !errors.Is(err, session.ErrMaxSessions) {
		t.Fatalf("SelectForNewSession() error = %v, want %v", err, session.ErrMaxSessions)
	}
}

func TestSlotGroup_ReachableServerIsSelected(t *testing.T) {
	now := time.Now()
	unreachable := &server.ManagedServer{State: server.StateRunning, StartedAt: now}
	reachable := &server.ManagedServer{State: server.StateRunning, StartedAt: now}
	manager1 := session.NewManager("slot-1", &config.ServerConfig{Command: "echo"}, nil)
	manager2 := session.NewManager("slot-2", &config.ServerConfig{Command: "echo"}, nil)
	mx := slots.NewMultiplexer("group", nil, []slots.Entry{
		{SlotName: "slot-1", Index: 1, Manager: manager1, Healthy: func() bool {
			return slotServerHealthy(unreachable, reachability.Reachability{State: reachability.StateUnreachable}, admin.DefaultReachabilityGrace)
		}},
		{SlotName: "slot-2", Index: 2, Manager: manager2, Healthy: func() bool {
			return slotServerHealthy(reachable, reachability.Reachability{State: reachability.StateReachable}, admin.DefaultReachabilityGrace)
		}},
	})

	selected, err := mx.SelectForNewSession(context.Background(), "upstream-1")
	if err != nil {
		t.Fatalf("SelectForNewSession() error = %v", err)
	}
	if selected != manager2 {
		t.Fatalf("selected manager = %v, want reachable slot", selected)
	}
}
