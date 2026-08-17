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
