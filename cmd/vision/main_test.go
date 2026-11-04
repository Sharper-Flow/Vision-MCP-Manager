package main

import (
	"strings"
	"testing"
)

func TestDaemonStatusPayloadIncludesHealthUptime(t *testing.T) {
	payload := daemonStatusPayload(true, 1234, map[string]interface{}{
		"status":    "ok",
		"uptime":    "5s",
		"memory_mb": 130.4,
	})

	if got := payload["running"]; got != true {
		t.Errorf("running = %v, want true", got)
	}
	if got := payload["pid"]; got != 1234 {
		t.Errorf("pid = %v, want 1234", got)
	}
	if got := payload["status"]; got != "ok" {
		t.Errorf("status = %v, want ok", got)
	}
	if got := payload["uptime"]; got != "5s" {
		t.Errorf("uptime = %v, want 5s", got)
	}
	if got := payload["memory_mb"]; got != 130.4 {
		t.Errorf("memory_mb = %v, want 130.4", got)
	}
}

func TestDaemonStatusPayloadOmitsEmptyHealthUptime(t *testing.T) {
	payload := daemonStatusPayload(true, 1234, map[string]interface{}{"uptime": ""})
	if _, ok := payload["uptime"]; ok {
		t.Fatalf("empty uptime must not be emitted: %#v", payload)
	}
}

func TestDaemonStatusPayloadOmitsMissingHealthMemory(t *testing.T) {
	payload := daemonStatusPayload(true, 1234, map[string]interface{}{"status": "ok"})
	if _, ok := payload["memory_mb"]; ok {
		t.Fatalf("missing memory must not be emitted: %#v", payload)
	}
}

func TestWriteDaemonStatusOmitsAbsentFieldsUnderVersionSkew(t *testing.T) {
	// Old daemons predate the /health uptime and memory fields.
	var out strings.Builder
	writeDaemonStatus(&out, true, 1234, map[string]interface{}{"status": "ok"})
	want := "Daemon is running (PID 1234)\n  Status: ok\n"
	if out.String() != want {
		t.Fatalf("output = %q, want %q", out.String(), want)
	}
}

func TestWriteDaemonStatusListsDegradedServers(t *testing.T) {
	var out strings.Builder
	writeDaemonStatus(&out, true, 1234, map[string]interface{}{
		"status":    "degraded",
		"uptime":    "5s",
		"memory_mb": 40.7,
		"errors":    []interface{}{"exa: probe failure at listener", "kagi: process failed"},
	})
	want := "Daemon is running (PID 1234)\n" +
		"  Status: degraded\n" +
		"  Uptime: 5s\n" +
		"  Memory: 40.7 MB\n" +
		"  Servers needing attention:\n" +
		"    exa: probe failure at listener\n" +
		"    kagi: process failed\n"
	if out.String() != want {
		t.Fatalf("output = %q, want %q", out.String(), want)
	}
}

func TestDaemonStatusPayloadIncludesHealthErrors(t *testing.T) {
	problems := []interface{}{"exa: probe failure at listener"}
	payload := daemonStatusPayload(true, 1234, map[string]interface{}{"status": "degraded", "errors": problems})
	if got, ok := payload["errors"].([]interface{}); !ok || len(got) != 1 || got[0] != problems[0] {
		t.Fatalf("errors = %#v, want %#v", payload["errors"], problems)
	}
}
