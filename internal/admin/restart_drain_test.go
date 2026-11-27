package admin

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/config"
	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/server"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestAdminRestartDrainWire(t *testing.T) {
	for _, tc := range []struct {
		name    string
		drain   *RestartDrain
		err     error
		want    any
		success bool
	}{
		{"drained", &RestartDrain{Outcome: "drained", TimeoutMS: 5000}, nil,
			map[string]any{"outcome": "drained", "timeout_ms": float64(5000), "remaining_requests": []any{}}, true},
		{"timed out", &RestartDrain{Outcome: "timed_out", TimeoutMS: 5000, RemainingRequests: []uint64{7, 19}}, nil,
			map[string]any{"outcome": "timed_out", "timeout_ms": float64(5000), "remaining_requests": []any{float64(7), float64(19)}}, true},
		{"restart error", &RestartDrain{Outcome: "timed_out", TimeoutMS: 5000, RemainingRequests: []uint64{7}}, errors.New("restart failed"),
			map[string]any{"outcome": "timed_out", "timeout_ms": float64(5000), "remaining_requests": []any{float64(7)}}, false},
		{"non-shared", nil, nil, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			registry := server.NewRegistry(nil, nil)
			t.Cleanup(registry.Close)
			if err := registry.Add("fixture", &config.ServerConfig{Command: "unused", Transport: config.TransportStdio, Port: 1234}); err != nil {
				t.Fatal(err)
			}
			called := false
			s := NewServer(Config{Registry: registry, RestartServer: func(ctx context.Context, name string) (*RestartDrain, error) {
				called = true
				if ctx == nil || name != "fixture" {
					t.Errorf("restart arguments: ctx=%v name=%q", ctx, name)
				}
				return tc.drain, tc.err
			}})
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			t.Cleanup(cancel)
			registered := mcp.NewServer(&mcp.Implementation{Name: "admin", Version: "1"}, nil)
			s.registerTools(registered)
			serverTransport, clientTransport := mcp.NewInMemoryTransports()
			serverSession, err := registered.Connect(ctx, serverTransport, nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = serverSession.Close() })
			client := mcp.NewClient(&mcp.Implementation{Name: "client", Version: "1"}, nil)
			session, err := client.Connect(ctx, clientTransport, nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = session.Close() })

			tools := outputTools(t, session, ctx)
			_, schema := outputSchema(t, tools["vision_restart"])
			result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "vision_restart", Arguments: map[string]any{"name": "fixture"}})
			if err != nil || result == nil || result.IsError {
				t.Fatalf("call: result=%#v err=%v", result, err)
			}
			text := result.Content[0].(*mcp.TextContent).Text
			object := outputParity(t, result, text)
			if err := schema.Validate(object); err != nil {
				t.Fatalf("wire payload rejected by schema: %v", err)
			}
			if !called || object["success"] != tc.success || object["name"] != "fixture" {
				t.Fatalf("unexpected restart response: called=%v response=%#v", called, object)
			}
			got, present := object["drain"]
			if present != (tc.want != nil) || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("drain=%#v present=%v, want %#v", got, present, tc.want)
			}
			if tc.err != nil && !strings.Contains(object["error"].(string), "Failed to restart server: restart failed") {
				t.Fatalf("restart error missing: %#v", object)
			}
			if tc.success && (object["status"] != "stopped" || object["process_state"] != "stopped" || object["port"] != float64(1234)) {
				t.Fatalf("restart status changed: %#v", object)
			}
		})
	}
}

func TestAdminRestartFallbackAndValidation(t *testing.T) {
	registry := server.NewRegistry(nil, nil)
	t.Cleanup(registry.Close)
	if err := registry.Add("fixture", &config.ServerConfig{Command: "unused", Transport: config.TransportStdio}); err != nil {
		t.Fatal(err)
	}
	s := NewServer(Config{Registry: registry})
	result, err := s.toolRestart(context.Background(), []byte(`{"name":"fixture"}`))
	if err != nil {
		t.Fatal(err)
	}
	var response RestartResponse
	decodeToolJSON(t, result, &response)
	if !response.Success || response.ProcessState != "running" || response.Error != nil || response.Drain != nil {
		t.Fatalf("fallback response=%#v", response)
	}
	called := false
	s = NewServer(Config{Registry: registry, RestartServer: func(context.Context, string) (*RestartDrain, error) {
		called = true
		return nil, nil
	}})
	if _, err := s.toolRestart(context.Background(), []byte(`{}`)); !isValidationError(err) {
		t.Fatalf("missing name error=%v", err)
	}
	result, err = s.toolRestart(context.Background(), []byte(`{"name":"missing"}`))
	if err != nil {
		t.Fatal(err)
	}
	response = RestartResponse{}
	decodeToolJSON(t, result, &response)
	if called || response.Success || response.Error == nil || !strings.Contains(*response.Error, "is not configured") {
		t.Fatalf("unknown server: called=%v response=%#v", called, response)
	}
}
