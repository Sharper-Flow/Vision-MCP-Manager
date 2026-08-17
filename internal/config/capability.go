package config

// SettingKey identifies a ServerConfig setting whose support depends on the
// server transport.
type SettingKey string

const (
	SettingSharedReadOnlyTools   SettingKey = "shared_read_only_tools"
	SettingSharedResultCacheTTL  SettingKey = "shared_result_cache_ttl"
	SettingSharedResultCacheSize SettingKey = "shared_result_cache_size"
	SettingMaxInFlightRequests   SettingKey = "max_in_flight_requests"
	SettingIdleReapTimeout       SettingKey = "idle_reap_timeout"

	// SettingDisconnectGracePeriod is honored by managed-http: daemon.go feeds
	// it to managed_gateway.go through ResolvedDisconnectGracePeriod().
	SettingDisconnectGracePeriod SettingKey = "disconnect_grace_period"
)

// Disposition describes whether a transport reads and applies a setting.
type Disposition int

const (
	DispositionHonored Disposition = iota
	DispositionRefused
)

// RefusalReason explains why a transport refuses a setting.
type RefusalReason int

const (
	ReasonNone RefusalReason = iota
	// ReasonSessionIsolation means cross-session sharing breaks the transport's
	// isolation guarantee.
	ReasonSessionIsolation
	// ReasonSupersededKnob means the transport implements the concern through a
	// different, honored setting.
	ReasonSupersededKnob
	// ReasonNotImplemented means the transport's proxy path does not read the
	// setting.
	ReasonNotImplemented
)

type settingCapability struct {
	disposition Disposition
	reason      RefusalReason
}

// GovernedSettingKeys is the fixed user-visible order for capability checks.
// Consumers must iterate this slice rather than a map: map iteration order is
// randomized, and error output must remain deterministic when multiple
// settings violate a transport's capabilities.
var GovernedSettingKeys = []SettingKey{
	SettingSharedReadOnlyTools,
	SettingSharedResultCacheTTL,
	SettingSharedResultCacheSize,
	SettingMaxInFlightRequests,
	SettingIdleReapTimeout,
	SettingDisconnectGracePeriod,
}

// transportCapabilities is the single source of truth for validation and
// availability-profile defaulting.
//
// DispositionHonored means "this change does not refuse the setting here". It
// is a policy statement, not a claim that the transport consumes the value.
// That distinction matters for http and sse: setupProxyForServer returns early
// for every non-stdio transport, because Vision does not proxy externally owned
// HTTP servers at all, so none of the governed settings reach a consumer on
// those transports either. They are recorded as honored only to preserve
// existing behavior -- refusing them would be a second, wider breaking change
// than the one this change was approved to make, and it would need to cover the
// whole server-level tuning surface rather than these six settings, since
// request_timeout, retry, circuit_breaker and health_check_interval are equally
// unreachable there. That wider question is a follow-up, recorded alongside the
// deferred settings below.
//
// The refusal check reads post-default values. An explicit user-written zero
// is therefore indistinguishable from unset and is not refused. That is
// accepted because zero means unlimited or use-default for every governed
// setting; no honorable intent is lost. Negative values are non-zero and are
// refused, as they express real intent (for example, idle_reap_timeout: -1
// means never reap).
var transportCapabilities = map[TransportType]map[SettingKey]settingCapability{
	TransportStdio: {
		SettingSharedReadOnlyTools:   {disposition: DispositionHonored, reason: ReasonNone},
		SettingSharedResultCacheTTL:  {disposition: DispositionHonored, reason: ReasonNone},
		SettingSharedResultCacheSize: {disposition: DispositionHonored, reason: ReasonNone},
		SettingMaxInFlightRequests:   {disposition: DispositionHonored, reason: ReasonNone},
		SettingIdleReapTimeout:       {disposition: DispositionHonored, reason: ReasonNone},
		SettingDisconnectGracePeriod: {disposition: DispositionHonored, reason: ReasonNone},
	},
	TransportHTTP: {
		SettingSharedReadOnlyTools:   {disposition: DispositionHonored, reason: ReasonNone},
		SettingSharedResultCacheTTL:  {disposition: DispositionHonored, reason: ReasonNone},
		SettingSharedResultCacheSize: {disposition: DispositionHonored, reason: ReasonNone},
		SettingMaxInFlightRequests:   {disposition: DispositionHonored, reason: ReasonNone},
		SettingIdleReapTimeout:       {disposition: DispositionHonored, reason: ReasonNone},
		SettingDisconnectGracePeriod: {disposition: DispositionHonored, reason: ReasonNone},
	},
	TransportSSE: {
		SettingSharedReadOnlyTools:   {disposition: DispositionHonored, reason: ReasonNone},
		SettingSharedResultCacheTTL:  {disposition: DispositionHonored, reason: ReasonNone},
		SettingSharedResultCacheSize: {disposition: DispositionHonored, reason: ReasonNone},
		SettingMaxInFlightRequests:   {disposition: DispositionHonored, reason: ReasonNone},
		SettingIdleReapTimeout:       {disposition: DispositionHonored, reason: ReasonNone},
		SettingDisconnectGracePeriod: {disposition: DispositionHonored, reason: ReasonNone},
	},
	TransportManagedHTTP: {
		SettingSharedReadOnlyTools:   {disposition: DispositionRefused, reason: ReasonSessionIsolation},
		SettingSharedResultCacheTTL:  {disposition: DispositionRefused, reason: ReasonSessionIsolation},
		SettingSharedResultCacheSize: {disposition: DispositionRefused, reason: ReasonSessionIsolation},
		SettingMaxInFlightRequests:   {disposition: DispositionRefused, reason: ReasonNotImplemented},
		SettingIdleReapTimeout:       {disposition: DispositionRefused, reason: ReasonSupersededKnob},
		SettingDisconnectGracePeriod: {disposition: DispositionHonored, reason: ReasonNone},
	},
}

// ungovernedSettings records every ServerConfig yaml field that is deliberately
// outside the capability table, mapped to the reason it is outside.
//
// The rationale is required, not decorative. This change exists because five
// settings were accepted for a transport that never read them, and nothing said
// so. An allowlist that took bare field names would let the next such setting be
// silenced by adding one line, which is the same failure with extra steps. The
// completeness test rejects an empty rationale, so opting a field out costs a
// written justification that a reviewer can disagree with.
var ungovernedSettings = map[SettingKey]string{
	// Identity and wiring. Every transport reads these, or fails validation
	// without them.
	"port":      "every transport binds a listener on it",
	"transport": "selects which capability row applies; it cannot be governed by the table it selects",
	"command":   "required by stdio and managed-http, rejected with url by the others at validation",
	"args":      "passed to the spawned process wherever a command is spawned",
	"env":       "passed to the spawned process wherever a command is spawned",
	"url":       "required by http, sse, and managed-http, and validated per transport already",
	"headers":   "forwarded by every transport that dials an upstream url",

	// Process lifecycle. Owned by the supervisor, which runs above the
	// transport and treats all transports alike.
	"autostart":      "supervisor-level, applied before any transport is constructed",
	"restart_policy": "supervisor-level restart handling, identical across transports",
	"max_restarts":   "supervisor-level restart budget, identical across transports",
	"stateful":       "process-per-session toggle; managed-http rejects it explicitly at validation",

	// Session lifecycle read by every transport's session or lease manager.
	"session_timeout": "honored everywhere; on managed-http it is the setting that supersedes idle_reap_timeout",
	"max_sessions":    "honored everywhere, including the managed-http lease admission limit",

	// Descriptive metadata, never interpreted.
	"availability_profile": "selects default values rather than behavior; the defaults it applies are themselves capability-gated",
	"required":             "informational for daemon startup and external tooling, not transport behavior",
	"source":               "informational only, preserved on round-trip and never interpreted",
	"description":          "informational only, preserved on round-trip and never interpreted",
	"request_timeout":      "applied by the shared HTTP client layer beneath every transport",

	// KNOWN SAME-CLASS DEFECTS -- deferred, not benign.
	//
	// Each of these is accepted on managed-http and never read there:
	// setupManagedHTTPProxy and ManagedHTTPGatewayConfig reference none of
	// them, and managed-http handles failure by recycling on ambiguity rather
	// than by retrying or circuit-breaking. They are the same defect this
	// change fixes for five other settings.
	//
	// They are ungoverned only because the approved agreement for
	// fixSilentlyIgnoredManagedHttp scoped it to those five. Refusing or
	// implementing these is a follow-up recorded in that change's design D6.
	// Extending the fix is a capability-table row plus a guard in
	// applyAvailabilityProfileDefaults -- note that the retry defaults
	// dereference s.Retry immediately after allocating it, so a guard must wrap
	// the whole retry block rather than each statement.
	"health_check_interval": "DEFERRED SAME-CLASS DEFECT: unread on managed-http and injected there by the networked profile; refusing it is out of scope for the change that added this table (see design D6)",
	"retry":                 "DEFERRED SAME-CLASS DEFECT: unread on managed-http and injected there by the networked profile; refusing it is out of scope for the change that added this table (see design D6)",
	"circuit_breaker":       "DEFERRED SAME-CLASS DEFECT: unread on managed-http and injected there by the networked profile; refusing it is out of scope for the change that added this table (see design D6)",
	"session_ttl":           "DEFERRED SAME-CLASS DEFECT: unread on managed-http, though unlike the others it is not profile-injected; refusing it is out of scope for the change that added this table (see design D6)",
}

func lookupCapability(transport TransportType, setting SettingKey) (Disposition, RefusalReason, bool) {
	settings, ok := transportCapabilities[transport]
	if !ok {
		return DispositionHonored, ReasonNone, false
	}
	capability, ok := settings[setting]
	if !ok {
		return DispositionHonored, ReasonNone, false
	}
	return capability.disposition, capability.reason, true
}
