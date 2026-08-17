package main

import "testing"

func TestDaemonStatusPayloadIncludesHealthUptime(t *testing.T) {
	payload := daemonStatusPayload(true, 1234, map[string]interface{}{
		"status": "ok",
		"uptime": "5s",
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
}

func TestDaemonStatusPayloadOmitsEmptyHealthUptime(t *testing.T) {
	payload := daemonStatusPayload(true, 1234, map[string]interface{}{"uptime": ""})
	if _, ok := payload["uptime"]; ok {
		t.Fatalf("empty uptime must not be emitted: %#v", payload)
	}
}
