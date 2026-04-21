package admin

import (
	"regexp"
	"strings"
)

// scrubberSet compiles the secret patterns once at package init.
// Patterns target common credential-bearing key/value and header forms
// seen in process error output.
type scrubberSet struct {
	patterns []*regexp.Regexp
}

var secretScrubber = buildScrubber()

func buildScrubber() scrubberSet {
	// Each pattern captures two groups: (key-or-scheme, separator+whitespace)
	// followed by the value to redact. We reconstruct the prefix and swap
	// the value for the redaction marker.
	patterns := []string{
		// Authorization: Bearer <token>   | Authorization=<token>
		`(?i)(authorization)\s*[:=]\s*(?:bearer\s+)?(\S+)`,
		// KEY_NAME=value where KEY_NAME ends with TOKEN/KEY/SECRET/PASSWORD/CREDENTIAL/PRIVATE/AUTH
		`(?i)([A-Z][A-Z0-9_]*(?:TOKEN|KEY|SECRET|PASSWORD|CREDENTIAL|PRIVATE|AUTH))\s*[:=]\s*(\S+)`,
		// password=<value> (lowercased form not already caught above)
		`(?i)(password)\s*=\s*(\S+)`,
	}
	var compiled []*regexp.Regexp
	for _, p := range patterns {
		compiled = append(compiled, regexp.MustCompile(p))
	}
	return scrubberSet{patterns: compiled}
}

// scrub applies every compiled pattern to the input string, redacting
// values while preserving the key names so operators can correlate.
func (s scrubberSet) scrub(in string) string {
	out := in
	for _, re := range s.patterns {
		out = re.ReplaceAllStringFunc(out, func(match string) string {
			// Extract the leading key portion via the first sub-match.
			sub := re.FindStringSubmatch(match)
			if len(sub) < 2 {
				return "***REDACTED***"
			}
			key := sub[1]
			// Determine separator used in the original match.
			sep := "="
			if strings.Contains(match, ":") {
				sep = ": "
			}
			return key + sep + "***REDACTED***"
		})
	}
	return out
}
