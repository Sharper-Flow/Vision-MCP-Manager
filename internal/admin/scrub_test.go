package admin

import (
	"strings"
	"testing"
)

// TestScrubSecrets_RedactsCommonCredentialPatterns verifies that the
// scrubber removes bearer tokens, suffixed env-like keys, and lowercase
// password=... forms from error strings before JSON serialization.
func TestScrubSecrets_RedactsCommonCredentialPatterns(t *testing.T) {
	cases := []struct {
		name       string
		in         string
		mustRedact []string // substrings that should be GONE from output
		mustKeep   []string // substrings that should STAY (e.g. key names)
	}{
		{
			name:       "authorization bearer",
			in:         "spawn failed: upstream returned Authorization: Bearer sk-abc123xyz (401)",
			mustRedact: []string{"sk-abc123xyz"},
			mustKeep:   []string{"Authorization"},
		},
		{
			name:       "suffixed env var assignment",
			in:         "env: KAGI_API_KEY=kgi_live_9999 not recognized",
			mustRedact: []string{"kgi_live_9999"},
			mustKeep:   []string{"KAGI_API_KEY"},
		},
		{
			name:       "generic token key",
			in:         "GITHUB_TOKEN=ghp_deadbeef0001 was rejected",
			mustRedact: []string{"ghp_deadbeef0001"},
			mustKeep:   []string{"GITHUB_TOKEN"},
		},
		{
			name:       "secret suffix",
			in:         "config error: STRIPE_SECRET=sk_test_zzz invalid",
			mustRedact: []string{"sk_test_zzz"},
			mustKeep:   []string{"STRIPE_SECRET"},
		},
		{
			name:       "credential suffix",
			in:         "auth failed: AWS_CREDENTIAL=AKIAFAKEFAKEFAKE rejected",
			mustRedact: []string{"AKIAFAKEFAKEFAKE"},
			mustKeep:   []string{"AWS_CREDENTIAL"},
		},
		{
			name:       "private suffix",
			in:         "config error: SSH_PRIVATE=-----BEGIN-RSA----- invalid",
			mustRedact: []string{"-----BEGIN-RSA-----"},
			mustKeep:   []string{"SSH_PRIVATE"},
		},
		{
			name:       "auth suffix",
			in:         "header parse: X_AUTH=deadbeefcafebabe failed",
			mustRedact: []string{"deadbeefcafebabe"},
			mustKeep:   []string{"X_AUTH"},
		},
		{
			name:       "lowercase password",
			in:         "db connect error: password=correcthorsebattery (user=admin)",
			mustRedact: []string{"correcthorsebattery"},
			mustKeep:   []string{"password"},
		},
		{
			name:       "benign string unchanged",
			in:         "timeout after 30s contacting upstream",
			mustRedact: nil,
			mustKeep:   []string{"timeout", "30s", "upstream"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := scrubSecrets(tc.in)
			for _, needle := range tc.mustRedact {
				if strings.Contains(got, needle) {
					t.Errorf("secret %q leaked through scrubber:\n  in:  %s\n  out: %s", needle, tc.in, got)
				}
			}
			for _, needle := range tc.mustKeep {
				if !strings.Contains(got, needle) {
					t.Errorf("expected %q to remain in output:\n  in:  %s\n  out: %s", needle, tc.in, got)
				}
			}
			if len(tc.mustRedact) > 0 && !strings.Contains(got, "***REDACTED***") {
				t.Errorf("expected ***REDACTED*** marker, got: %s", got)
			}
		})
	}
}

// TestScrubSecrets_EmptyIsNoop guards the fast-path for empty error strings.
func TestScrubSecrets_EmptyIsNoop(t *testing.T) {
	if got := scrubSecrets(""); got != "" {
		t.Errorf("scrubSecrets(\"\") = %q, want empty", got)
	}
}
