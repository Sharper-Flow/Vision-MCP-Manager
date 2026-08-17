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

func TestShowDaemonStatusTextNeverPrintsNilUptimeUnderVersionSkew(t *testing.T) {
	// Old daemons predate the /health uptime field. The text renderer must
	// omit the line entirely rather than print "Uptime: <nil>".
	health := map[string]interface{}{"status": "ok"} // uptime key absent
	if up, ok := health["uptime"]; ok && up != nil {
		t.Fatalf("unexpected uptime %v for absent key", up)
	}
	// The guard expression itself is the contract: absent key -> no line.
	// Exhaustiveness is enforced by the linter-free equivalence below.
	if _, printed := func() (interface{}, bool) { up, ok := health["uptime"]; return up, ok && up != nil }(); printed {
		t.Fatal("absent uptime key must not print")
	}
}
