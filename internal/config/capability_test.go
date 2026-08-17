package config

import (
	"errors"
	"strings"
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
// defaults declaring managed-http, so expansion must refuse the group before
// synthesizing members.
func TestSlotGroupSynthesizedManagedHTTPIsStillRefused(t *testing.T) {
	cfg := &Config{SlotGroups: map[string]*SlotGroupConfig{
		"browser-pool": {
			Template:  "browser",
			BasePort:  6290,
			Count:     2,
			GroupPort: 6289,
			Defaults: &ServerConfig{
				Transport:           TransportManagedHTTP,
				Command:             "npx",
				URL:                 "http://127.0.0.1:16290/mcp",
				MaxInFlightRequests: 4,
			},
		},
	}}
	err := expandSlotGroups(cfg)
	if err == nil {
		t.Fatal("expandSlotGroups() must refuse managed-http slot group defaults")
	}
	if !errors.Is(err, ErrConflictingConfig) {
		t.Fatalf("error = %v, want ErrConflictingConfig", err)
	}
	if !strings.Contains(err.Error(), string(TransportManagedHTTP)) {
		t.Fatalf("error = %v, want transport %q", err, TransportManagedHTTP)
	}
}

func TestExpandSlotGroupsDefaultTransportMustBeStdio(t *testing.T) {
	tests := []struct {
		name            string
		defaults        *ServerConfig
		wantErr         bool
		wantTransport   TransportType
		wantMemberError error
	}{
		{
			name: "explicit managed-http",
			defaults: &ServerConfig{
				Transport: TransportManagedHTTP,
				Command:   "npx",
				URL:       "http://127.0.0.1:16290/mcp",
			},
			wantErr:       true,
			wantTransport: TransportManagedHTTP,
		},
		{
			name: "url only with mcp suffix",
			defaults: &ServerConfig{
				URL: "http://127.0.0.1:16290/mcp",
			},
			wantErr:       true,
			wantTransport: TransportHTTP,
		},
		{
			name: "url only without mcp suffix",
			defaults: &ServerConfig{
				URL: "http://127.0.0.1:16290/events",
			},
			wantErr:       true,
			wantTransport: TransportSSE,
		},
		{
			name: "command and url infer stdio",
			defaults: &ServerConfig{
				Command: "npx",
				URL:     "http://127.0.0.1:16290/mcp",
			},
			wantMemberError: ErrConflictingConfig,
		},
		{
			name: "stdio command only",
			defaults: &ServerConfig{
				Command: "npx",
			},
		},
		{
			name:            "nil defaults",
			wantMemberError: ErrMissingCommand,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{SlotGroups: map[string]*SlotGroupConfig{
				"browser-pool": {
					Template: "browser",
					BasePort: 6290,
					Count:    2,
					Defaults: tt.defaults,
				},
			}}

			err := expandSlotGroups(cfg)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expandSlotGroups() error = nil, want refusal")
				}
				if !errors.Is(err, ErrConflictingConfig) {
					t.Fatalf("error = %v, want ErrConflictingConfig", err)
				}
				if !strings.Contains(err.Error(), string(tt.wantTransport)) {
					t.Fatalf("error = %v, want transport %q", err, tt.wantTransport)
				}
				return
			}
			if err != nil {
				t.Fatalf("expandSlotGroups() error = %v", err)
			}

			synthesized := cfg.Servers["browser-1"]
			if synthesized == nil {
				t.Fatal("expandSlotGroups() did not synthesize browser-1")
			}
			if tt.defaults != nil && tt.defaults.Command != "" && synthesized.Command != tt.defaults.Command {
				t.Fatalf("synthesized command = %q, want %q", synthesized.Command, tt.defaults.Command)
			}
			if tt.wantMemberError != nil {
				memberErr := synthesized.Validate("browser-pool-1")
				if memberErr == nil {
					t.Fatal("synthesized member validation error = nil")
				}
				if !errors.Is(memberErr, tt.wantMemberError) {
					t.Fatalf("synthesized member error = %v, want %v", memberErr, tt.wantMemberError)
				}
			}
		})
	}
}

// TestNonManagedTransportsAcceptGovernedSettings guards the blast radius: this
// change must not start refusing governed settings on stdio, http, or sse.
func TestNonManagedTransportsAcceptGovernedSettings(t *testing.T) {
	for _, transport := range []TransportType{TransportStdio, TransportHTTP, TransportSSE} {
		for _, setting := range GovernedSettingKeys {
			disposition, reason, ok := lookupCapability(transport, setting)
			if !ok {
				t.Errorf("missing capability entry for transport %q, setting %q", transport, setting)
				continue
			}
			if disposition != DispositionHonored {
				t.Errorf("transport %q must accept %q, got disposition %v", transport, setting, disposition)
			}
			if reason != ReasonNone {
				t.Errorf("transport %q accepts %q so reason must be ReasonNone, got %v", transport, setting, reason)
			}
		}
	}
}
