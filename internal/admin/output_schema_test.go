package admin

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/config"
	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/metrics"
	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/reachability"
	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/server"
	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func outputJSON(t *testing.T, value any) []byte {
	t.Helper()
	b, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func outputObject(t *testing.T, value any) map[string]any {
	t.Helper()
	var object map[string]any
	if err := json.Unmarshal(outputJSON(t, value), &object); err != nil || object == nil {
		t.Fatalf("not a JSON object: %#v (%v)", value, err)
	}
	return object
}

func outputFixture(t *testing.T, s *Server) (*mcp.ClientSession, context.Context, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	registered := mcp.NewServer(&mcp.Implementation{Name: "admin-contract", Version: "1"}, nil)
	s.registerTools(registered)
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return registered
	}, &mcp.StreamableHTTPOptions{JSONResponse: true})
	listener := httptest.NewServer(handler)
	t.Cleanup(listener.Close)
	client := mcp.NewClient(&mcp.Implementation{Name: "admin-contract-client", Version: "1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: listener.URL}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session, ctx, listener.URL
}

func outputTools(t *testing.T, session *mcp.ClientSession, ctx context.Context) map[string]*mcp.Tool {
	t.Helper()
	listed, err := session.ListTools(ctx, &mcp.ListToolsParams{})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed.Tools) != 10 {
		t.Fatalf("got %d tools, want all ten", len(listed.Tools))
	}
	tools := make(map[string]*mcp.Tool, len(listed.Tools))
	for _, tool := range listed.Tools {
		if tools[tool.Name] != nil {
			t.Fatalf("duplicate tool %q", tool.Name)
		}
		tools[tool.Name] = tool
	}
	return tools
}

func outputSchema(t *testing.T, tool *mcp.Tool) (*jsonschema.Schema, *jsonschema.Resolved) {
	t.Helper()
	if tool == nil || tool.OutputSchema == nil {
		t.Fatal("registered tool has no output schema")
	}
	var schema jsonschema.Schema
	if err := json.Unmarshal(outputJSON(t, tool.OutputSchema), &schema); err != nil {
		t.Fatal(err)
	}
	if schema.Type != "object" || len(schema.Properties) == 0 || len(schema.Required) == 0 {
		t.Fatalf("output schema is not a concrete root-object contract: %s", outputJSON(t, tool.OutputSchema))
	}
	resolved, err := schema.Resolve(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &schema, resolved
}

func outputPayloads() map[string][]any {
	message, status, count := "fixture error", "reachable", 7
	stamp := time.Unix(1700000000, 0).UTC()
	reach := ReachabilityDetails{Reachability: status, ProbeDepth: "listener", LastProbeAt: &stamp, LastProbeOutcome: "success", ConsecutiveProbeFailures: 1}
	snapshot := &metrics.ServerMetricsSnapshot{
		ActiveSessions: 1, ReapedByReason: map[string]int64{"idle_timeout": 2},
		SpawnedByReason:  map[string]int64{"first_use": 1},
		SharedDownstream: &metrics.SharedDownstreamSnapshot{PID: 42, StartedAt: &stamp},
	}
	return map[string][]any{
		"vision_list": {
			ListResponse{}, ListResponse{Servers: []ListServerEntry{}, SlotGroups: []SlotGroupEntry{}},
			ListResponse{Servers: []ListServerEntry{{Name: "fixture", CodemodeNamespace: "fixture", Status: "error", ProcessState: "failed", Port: &count, PID: &count, Uptime: &status, Error: &message, EffectiveReason: &message, ReachabilityDetails: reach, SessionMetrics: snapshot, SessionLifecycle: &SessionLifecycleSnapshot{BackendState: "ready", Sessions: []SessionLifecycleRow{{SafeID: "bounded", State: "active"}}, Closed: []SessionLifecycleRow{}}}}, SlotGroups: []SlotGroupEntry{{Name: "group", Slots: []string{"fixture"}}}},
			ListResponse{Servers: []ListServerEntry{{Name: "fixture", SessionMetrics: &metrics.ServerMetricsSnapshot{}}}},
		},
		"vision_add":         {AddResponse{}, AddResponse{Success: true, Name: "fixture", Status: "running", Port: &count, EffectiveReason: &message, ReachabilityDetails: reach}, AddResponse{Success: false, Name: "fixture", Error: &message}},
		"vision_remove":      {RemoveResponse{}, RemoveResponse{Success: true, Name: "fixture"}, RemoveResponse{Success: false, Name: "fixture", Error: &message}},
		"vision_restart":     {RestartResponse{}, RestartResponse{Success: true, Name: "fixture", Status: "running", ProcessState: "running", Port: &count, EffectiveReason: &message, ReachabilityDetails: reach}, RestartResponse{Success: false, Name: "fixture", Error: &message}},
		"vision_search":      {SearchResponse{}, SearchResponse{Success: true, Results: []SearchResultEntry{}}, SearchResponse{Success: true, Results: []SearchResultEntry{{Name: "fixture", CodemodeNamespace: "fixture", Capabilities: []string{"search"}, Installed: true}}}},
		"vision_init":        {InitResponse{}, InitResponse{Success: true, Path: "/fixture.jsonc", Servers: []string{"fixture"}, ReconciledServers: []string{"fixture"}, BackedUp: true, BackupPath: "/fixture.backup"}, InitResponse{Success: false, Path: "/fixture.jsonc", Error: &message}},
		"vision_status":      {StatusResponse{}, StatusResponse{Healthy: true, Uptime: "1m0s", Servers: StatusServers{Running: 1}, MemoryMB: 1.5, Warnings: []string{"fixture warning"}}},
		"vision_guidance":    {GuidanceResponse{}, GuidanceResponse{Success: true, Context: "search", GlobalGuidance: "fixture", Servers: []GuidanceEntry{{Name: "fixture", Priority: "high", PreferFor: []string{"search"}}}, Tools: []GuidanceEntry{{Name: "query", NamespacedName: "fixture_query", Guidance: "fixture", AvoidFor: []string{"other"}, Examples: []string{"query"}}}}, GuidanceResponse{Success: true, Error: &message}},
		"vision_slot_status": {SlotStatusResponse{}, SlotStatusResponse{Groups: []SlotGroupStatus{}}, SlotStatusResponse{Groups: []SlotGroupStatus{{GroupName: "group", SlotCount: 1, Slots: []SlotDetail{{Name: "fixture", Port: count, ActiveSessions: 1, MaxSessions: 3, EffectiveStatus: "running", EffectiveReason: &message, ReachabilityDetails: reach}}}}}},
		"vision_metrics":     {MetricsResponse{}, MetricsResponse{Servers: map[string]*metrics.ServerMetricsSnapshot{}}, MetricsResponse{Servers: map[string]*metrics.ServerMetricsSnapshot{"fixture": snapshot}}, MetricsResponse{Servers: map[string]*metrics.ServerMetricsSnapshot{"fixture": {}}}},
	}
}

func TestAdminOutputSchemas(t *testing.T) {
	s := NewServer(Config{})
	session, ctx, _ := outputFixture(t, s)
	tools := outputTools(t, session, ctx)
	for _, definition := range s.getTools() {
		t.Run(definition.Name, func(t *testing.T) {
			tool := tools[definition.Name]
			if tool == nil {
				t.Fatal("missing registered tool")
			}
			if !reflect.DeepEqual(outputObject(t, tool.InputSchema), outputObject(t, definition.InputSchema)) {
				t.Fatal("input schema changed during registration")
			}
			schema, resolved := outputSchema(t, tool)
			for _, payload := range outputPayloads()[definition.Name] {
				object := outputObject(t, payload)
				if err := resolved.Validate(object); err != nil {
					t.Errorf("schema rejects %s: %v", outputJSON(t, payload), err)
				}
				for key := range object {
					if schema.Properties[key] == nil {
						t.Errorf("payload field %q has no concrete property schema", key)
					}
				}
			}
			object := outputObject(t, outputPayloads()[definition.Name][0])
			delete(object, schema.Required[0])
			if err := resolved.Validate(object); err == nil {
				t.Error("schema accepts an omitted required field")
			}
			if err := resolved.Validate([]any{}); err == nil {
				t.Error("schema accepts an array root")
			}
		})
	}
}

func outputParity(t *testing.T, result *mcp.CallToolResult, text string) map[string]any {
	t.Helper()
	if result == nil || len(result.Content) != 1 {
		t.Fatalf("missing single text fallback: %#v", result)
	}
	fallback, ok := result.Content[0].(*mcp.TextContent)
	if !ok || fallback.Text != text {
		t.Fatalf("text bytes changed: %#v, want %s", result.Content[0], text)
	}
	structured := outputObject(t, result.StructuredContent)
	var expected map[string]any
	if err := json.Unmarshal([]byte(text), &expected); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(structured, expected) {
		t.Fatalf("structured/text mismatch: %#v versus %#v", structured, expected)
	}
	return structured
}

type outputMetrics struct{}

func (outputMetrics) ServerMetricsSnapshot(string) *metrics.ServerMetricsSnapshot {
	return &metrics.ServerMetricsSnapshot{}
}

func TestAdminOutputParity(t *testing.T) {
	for tool, payloads := range outputPayloads() {
		t.Run(tool, func(t *testing.T) {
			for _, payload := range payloads {
				result, err := jsonToolResult(payload)
				if err != nil {
					t.Fatal(err)
				}
				text := string(outputJSON(t, payload))
				converted := toMCPToolResult(result)
				outputParity(t, converted, text)
				if converted.IsError != result.IsError {
					t.Fatal("IsError changed")
				}
			}
		})
	}
	registry := server.NewRegistry(nil, nil)
	if err := registry.Add("fixture", &config.ServerConfig{Command: "unused", Transport: config.TransportStdio}); err != nil {
		t.Fatal(err)
	}
	s := NewServer(Config{Registry: registry, ServerMetricsAccessor: outputMetrics{}})
	session, ctx, _ := outputFixture(t, s)
	tools := outputTools(t, session, ctx)
	for _, name := range []string{"vision_list", "vision_metrics"} {
		t.Run(name+"/actual-nil-maps", func(t *testing.T) {
			result, err := s.callTool(ctx, name, json.RawMessage(`{}`))
			if err != nil {
				t.Fatal(err)
			}
			object := outputParity(t, toMCPToolResult(result), result.Content[0].Text)
			_, schema := outputSchema(t, tools[name])
			if err := schema.Validate(object); err != nil {
				t.Fatalf("actual handler payload does not validate: %v", err)
			}
			var snapshot map[string]any
			if name == "vision_list" {
				snapshot = object["servers"].([]any)[0].(map[string]any)["session_metrics"].(map[string]any)
			} else {
				snapshot = object["servers"].(map[string]any)["fixture"].(map[string]any)
			}
			for _, key := range []string{"reaped_by_reason", "spawned_by_reason"} {
				if value, present := snapshot[key]; !present || value != nil {
					t.Fatalf("%s must remain present and null, got %#v", key, snapshot)
				}
			}
		})
	}
}

func TestAdminOutputWire(t *testing.T) {
	s := NewServer(Config{Registry: server.NewRegistry(nil, nil)})
	first, ctx, firstURL := outputFixture(t, s)
	second, secondCtx, secondURL := outputFixture(t, NewServer(Config{}))
	if firstURL == secondURL {
		t.Fatal("fixtures do not own independent listeners")
	}
	tools := outputTools(t, first, ctx)
	outputTools(t, second, secondCtx)
	for name := range outputPayloads() {
		t.Run(name, func(t *testing.T) {
			args := map[string]any{}
			switch name {
			case "vision_add", "vision_remove", "vision_restart":
				args["name"] = "admin-acceptance-missing-server"
				if name == "vision_add" {
					args["start"] = false
				}
			case "vision_init":
				args["path"] = filepath.Join(t.TempDir(), "client.jsonc")
			}
			result, err := first.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
			if err != nil {
				t.Fatal(err)
			}
			text, ok := result.Content[0].(*mcp.TextContent)
			if !ok || result.IsError {
				t.Fatalf("unexpected success/business-error routing: %#v", result)
			}
			object := outputParity(t, result, text.Text)
			_, schema := outputSchema(t, tools[name])
			if err := schema.Validate(object); err != nil {
				t.Fatalf("wire payload rejected by advertised schema: %v", err)
			}
			if name == "vision_remove" || name == "vision_restart" || name == "vision_add" || name == "vision_init" {
				if object["success"] != false {
					t.Fatalf("business failure value changed: %#v", object)
				}
			}
		})
	}
	for _, name := range []string{"vision_add", "vision_remove", "vision_restart", "vision_init"} {
		if result, err := first.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: map[string]any{}}); err == nil || result != nil {
			t.Errorf("%s validation no longer returns a protocol error: result=%#v err=%v", name, result, err)
		}
	}
	result, err := second.CallTool(secondCtx, &mcp.CallToolParams{Name: "vision_list", Arguments: map[string]any{}})
	if err != nil || result == nil || !result.IsError || result.StructuredContent != nil {
		t.Fatalf("plain-text execution error routing changed: result=%#v err=%v", result, err)
	}
	if len(result.Content) != 1 || result.Content[0].(*mcp.TextContent).Text != "registry not initialized" {
		t.Fatalf("plain-text error changed: %#v", result.Content)
	}
	if got := toMCPToolResult(nil); len(got.Content) != 0 || got.StructuredContent != nil || got.IsError {
		t.Fatalf("nil result behavior changed: %#v", got)
	}
}

func TestAdminOutputScrubbing(t *testing.T) {
	registry := server.NewRegistry(nil, nil)
	if err := registry.Add("fixture", &config.ServerConfig{Command: "unused", Transport: config.TransportStdio}); err != nil {
		t.Fatal(err)
	}
	store := reachability.NewStore()
	for range reachability.FailureThreshold {
		store.RecordProbe("fixture", reachability.ProbeResult{Depth: reachability.DepthListener, Disposition: reachability.DispositionFailure, Error: "authorization=probe-secret AUTH_TOKEN=token-secret", AttemptedAt: time.Unix(1700000000, 0).UTC()})
	}
	s := NewServer(Config{Registry: registry, ReachabilityStore: store, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	result, err := s.callTool(context.Background(), "vision_list", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	converted := toMCPToolResult(result)
	object := outputParity(t, converted, result.Content[0].Text)
	for _, serialized := range []string{result.Content[0].Text, string(outputJSON(t, converted.StructuredContent))} {
		if strings.Contains(serialized, "probe-secret") || strings.Contains(serialized, "token-secret") || !strings.Contains(serialized, "REDACTED") {
			t.Fatalf("secret scrubbing changed: %s", serialized)
		}
	}
	entry := object["servers"].([]any)[0].(map[string]any)
	if !strings.Contains(entry["last_probe_error"].(string), "REDACTED") {
		t.Fatal("actual scrubbed probe error missing")
	}
}
