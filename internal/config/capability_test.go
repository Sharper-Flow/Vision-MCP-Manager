package config

import (
	"errors"
	"testing"
)

func TestCapabilityTableCoversAllGovernedSettings(t *testing.T) {
	transports := []TransportType{
		TransportStdio,
		TransportHTTP,
		TransportSSE,
		TransportManagedHTTP,
	}

	for _, transport := range transports {
		for _, setting := range GovernedSettingKeys {
			if _, _, ok := lookupCapability(transport, setting); !ok {
				t.Errorf("missing capability entry for transport %q, setting %q", transport, setting)
			}
		}
	}
}

// TestManagedHTTPDispositions pins the managed-http dispositions and reasons.
// The coverage test above proves every cell is present, not that any cell is
// correct: a table that honored everything on managed-http would still pass it.
// The reasons are pinned too, because they become user-facing message text --
// idle_reap_timeout must redirect to session_timeout rather than report itself
// unimplemented, since managed-http does implement the idle concern.
func TestManagedHTTPDispositions(t *testing.T) {
	want := map[SettingKey]settingCapability{
		SettingSharedReadOnlyTools:   {DispositionRefused, ReasonSessionIsolation},
		SettingSharedResultCacheTTL:  {DispositionRefused, ReasonSessionIsolation},
		SettingSharedResultCacheSize: {DispositionRefused, ReasonSessionIsolation},
		SettingMaxInFlightRequests:   {DispositionRefused, ReasonNotImplemented},
		SettingIdleReapTimeout:       {DispositionRefused, ReasonSupersededKnob},
		// Honored, and deliberately so: daemon.go feeds it to the managed-http
		// gateway via ResolvedDisconnectGracePeriod(). Refusing it would break
		// working behavior.
		SettingDisconnectGracePeriod: {DispositionHonored, ReasonNone},
	}

	for _, setting := range GovernedSettingKeys {
		expected, listed := want[setting]
		if !listed {
			t.Fatalf("governed setting %q has no pinned managed-http expectation", setting)
		}
		disposition, reason, ok := lookupCapability(TransportManagedHTTP, setting)
		if !ok {
			t.Errorf("missing managed-http capability entry for %q", setting)
			continue
		}
		if disposition != expected.disposition {
			t.Errorf("managed-http %q disposition = %v, want %v", setting, disposition, expected.disposition)
		}
		if reason != expected.reason {
			t.Errorf("managed-http %q reason = %v, want %v", setting, reason, expected.reason)
		}
	}
}

// TestSlotGroupSynthesizedManagedHTTPIsStillRefused pins the rule that support
// is keyed by a server's transport, not by how the server came to exist. Slot
// group members are synthesized from group defaults, and nothing stops those
// defaults declaring managed-http, so a synthesized member must be refused on
// exactly the same terms as a hand-written one. Getting this wrong would leave
// a synthesis-shaped hole in the fix.
func TestSlotGroupSynthesizedManagedHTTPIsStillRefused(t *testing.T) {
	synthesized := &ServerConfig{
		Port:                6290,
		Transport:           TransportManagedHTTP,
		Command:             "npx",
		URL:                 "http://127.0.0.1:16290/mcp",
		MaxInFlightRequests: 4,
		SlotGroup:           "browser-pool",
		SlotIndex:           1,
	}

	err := synthesized.Validate("browser-pool-1")
	if err == nil {
		t.Fatal("synthesized managed-http slot member must be refused, got nil")
	}
	if !errors.Is(err, ErrSettingNotSupportedByTransport) {
		t.Fatalf("error = %v, want ErrSettingNotSupportedByTransport", err)
	}
}

// TestNonManagedTransportsHonorGovernedSettings guards the blast radius: this
// change must not alter stdio, http, or sse behavior for any governed setting.
func TestNonManagedTransportsHonorGovernedSettings(t *testing.T) {
	for _, transport := range []TransportType{TransportStdio, TransportHTTP, TransportSSE} {
		for _, setting := range GovernedSettingKeys {
			disposition, reason, ok := lookupCapability(transport, setting)
			if !ok {
				t.Errorf("missing capability entry for transport %q, setting %q", transport, setting)
				continue
			}
			if disposition != DispositionHonored {
				t.Errorf("transport %q must honor %q, got disposition %v", transport, setting, disposition)
			}
			if reason != ReasonNone {
				t.Errorf("transport %q honors %q so reason must be ReasonNone, got %v", transport, setting, reason)
			}
		}
	}
}
