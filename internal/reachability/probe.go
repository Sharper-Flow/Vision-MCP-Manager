package reachability

import (
	"context"
	"fmt"
	"hash/fnv"
	"log/slog"
	"sync"
	"time"
)

const (
	ProtocolVersion2024_11_05 = "2024-11-05"
	ProtocolVersion2025_03_26 = "2025-03-26"
	ProtocolVersion2025_06_18 = "2025-06-18"
	ProtocolVersion2025_11_25 = "2025-11-25"

	DefaultProbeInterval = 30 * time.Second
	// EndToEndProbeIntervalMultiple keeps the admission-consuming probe rare
	// relative to the free listener probe.
	EndToEndProbeIntervalMultiple = 10
	probeTimeout                  = 5 * time.Second
)

// Target identifies the local Vision listener to probe. ProtocolVersion is
// the negotiated MCP revision; keeping it on the target makes the mechanism
// selection explicit when a later revision needs a different probe.
type Target struct {
	Name            string
	Port            int
	ProtocolVersion string
	BearerToken     string
}

// Probe is the version-independent seam for reachability mechanisms.
// Implementations return false with an error when the target is not reachable.
type Probe interface {
	Probe(context.Context, Target) (bool, error)
}

// VersionSelector maps negotiated MCP revisions to probe mechanisms. It is
// intentionally explicit: protocol revisions that remove initialize/sessions
// must not silently inherit a mechanism designed for an older revision.
type VersionSelector struct {
	probes         map[string]Probe
	endToEndProbes map[string]Probe
}

func NewVersionSelector(listener Probe, endToEnd ...Probe) *VersionSelector {
	selector := &VersionSelector{probes: map[string]Probe{
		ProtocolVersion2024_11_05: listener,
		ProtocolVersion2025_03_26: listener,
		ProtocolVersion2025_06_18: listener,
		ProtocolVersion2025_11_25: listener,
	}, endToEndProbes: make(map[string]Probe)}
	if len(endToEnd) > 0 && endToEnd[0] != nil {
		for version := range selector.probes {
			selector.endToEndProbes[version] = endToEnd[0]
		}
	}
	return selector
}

// Select returns the mechanism for a negotiated revision. An empty revision
// uses the latest revision supported by the current SDK, which is the only
// safe default before a server has completed negotiation.
func (s *VersionSelector) Select(protocolVersion string) (Probe, error) {
	if protocolVersion == "" {
		protocolVersion = ProtocolVersion2025_11_25
	}
	probe, ok := s.probes[protocolVersion]
	if !ok {
		return nil, fmt.Errorf("no reachability probe for MCP protocol revision %q", protocolVersion)
	}
	return probe, nil
}

func (s *VersionSelector) SelectEndToEnd(protocolVersion string) (Probe, error) {
	if protocolVersion == "" {
		protocolVersion = ProtocolVersion2025_11_25
	}
	probe, ok := s.endToEndProbes[protocolVersion]
	if !ok {
		if len(s.endToEndProbes) == 0 {
			return nil, nil
		}
		return nil, fmt.Errorf("no end-to-end reachability probe for MCP protocol revision %q", protocolVersion)
	}
	return probe, nil
}

type workerHandle struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// Manager owns one cancellable probe worker per server.
type Manager struct {
	store    *Store
	selector *VersionSelector
	logger   *slog.Logger

	mu      sync.Mutex
	workers map[string]*workerHandle
}

func NewManager(store *Store, selector *VersionSelector, logger ...*slog.Logger) *Manager {
	if store == nil {
		store = NewStore()
	}
	if selector == nil {
		selector = &VersionSelector{probes: make(map[string]Probe)}
	}
	log := slog.Default()
	if len(logger) > 0 && logger[0] != nil {
		log = logger[0]
	}
	return &Manager{store: store, selector: selector, logger: log, workers: make(map[string]*workerHandle)}
}

func (m *Manager) Start(parent context.Context, target Target, interval time.Duration) error {
	probe, err := m.selector.Select(target.ProtocolVersion)
	if err != nil {
		return err
	}
	endToEnd, err := m.selector.SelectEndToEnd(target.ProtocolVersion)
	if err != nil {
		return err
	}
	return m.startWithProbes(parent, target, interval, probe, endToEnd)
}

func (m *Manager) StartWithProbe(parent context.Context, target Target, interval time.Duration, probe Probe) error {
	return m.startWithProbes(parent, target, interval, probe, nil)
}

func (m *Manager) startWithProbes(parent context.Context, target Target, interval time.Duration, probe, endToEnd Probe) error {
	if target.Name == "" {
		return fmt.Errorf("probe target name is empty")
	}
	if probe == nil {
		return fmt.Errorf("probe for %q is nil", target.Name)
	}
	if interval <= 0 {
		interval = DefaultProbeInterval
	}
	// Ordered lifecycle events normally make this unnecessary, but replacing a
	// stale worker here prevents duplicate loops if a caller starts a server
	// twice without an intervening stop event.
	if err := m.Stop(target.Name); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(parent)
	handle := &workerHandle{cancel: cancel, done: make(chan struct{})}
	m.mu.Lock()
	m.workers[target.Name] = handle
	m.mu.Unlock()
	go func() {
		defer close(handle.done)
		defer m.finished(target.Name, handle)
		runProbeWorker(ctx, m.store, m.logger, target, interval, probe, endToEnd)
	}()
	return nil
}

func (m *Manager) finished(name string, handle *workerHandle) {
	m.mu.Lock()
	if m.workers[name] == handle {
		delete(m.workers, name)
	}
	m.mu.Unlock()
}

// Remove stops the worker before deleting evidence, ensuring an in-flight
// result cannot be written after the server has left the registry.
func (m *Manager) Remove(name string) error {
	if err := m.Stop(name); err != nil {
		return err
	}
	m.store.Remove(name)
	return nil
}

func (m *Manager) Stop(name string) error {
	m.mu.Lock()
	handle := m.workers[name]
	if handle != nil {
		delete(m.workers, name)
	}
	m.mu.Unlock()
	if handle == nil {
		return nil
	}
	handle.cancel()
	<-handle.done
	return nil
}

// Close cancels and joins every worker. It is safe to call repeatedly.
func (m *Manager) Close() {
	m.mu.Lock()
	handles := make([]*workerHandle, 0, len(m.workers))
	for name, handle := range m.workers {
		delete(m.workers, name)
		handles = append(handles, handle)
	}
	m.mu.Unlock()
	for _, handle := range handles {
		handle.cancel()
	}
	for _, handle := range handles {
		<-handle.done
	}
}

// Wait joins currently registered workers. Parent context cancellation causes
// workers to unregister, so this is useful during daemon shutdown as well.
func (m *Manager) Wait() {
	m.mu.Lock()
	handles := make([]*workerHandle, 0, len(m.workers))
	for _, handle := range m.workers {
		handles = append(handles, handle)
	}
	m.mu.Unlock()
	for _, handle := range handles {
		<-handle.done
	}
}

func runProbeWorker(ctx context.Context, store *Store, logger *slog.Logger, target Target, interval time.Duration, probe, endToEnd Probe) {
	first := time.NewTimer(firstTickDelay(target.Name, interval))
	defer first.Stop()
	select {
	case <-ctx.Done():
		return
	case <-first.C:
	}

	runProbe(ctx, store, logger, target, probe)
	deepInterval := interval * EndToEndProbeIntervalMultiple
	nextEndToEnd := time.Now().Add(deepInterval)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			runProbe(ctx, store, logger, target, probe)
			if endToEnd != nil && !time.Now().Before(nextEndToEnd) {
				runEndToEndProbe(ctx, store, logger, target, deepInterval, endToEnd)
				nextEndToEnd = time.Now().Add(deepInterval)
			}
		}
	}
}

func runProbe(ctx context.Context, store *Store, logger *slog.Logger, target Target, probe Probe) {
	runProbeAtDepth(ctx, store, logger, target, probe, DepthListener)
}

func runEndToEndProbe(ctx context.Context, store *Store, logger *slog.Logger, target Target, freshness time.Duration, probe Probe) {
	now := time.Now()
	if value, ok := store.Get(target.Name); ok {
		if evidence, ok := value.Evidence[DepthSession]; ok && !evidence.LastProbeAttempt.IsZero() && now.Before(evidence.LastProbeAttempt.Add(freshness)) {
			return
		}
	}
	runProbeAtDepth(ctx, store, logger, target, probe, DepthEndToEnd)
}

func runProbeAtDepth(ctx context.Context, store *Store, logger *slog.Logger, target Target, probe Probe, depth Depth) {
	attemptedAt := time.Now()
	store.StartProbe(target.Name, depth, attemptedAt)
	probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	reachable, err := probe.Probe(probeCtx, target)
	cancel()
	if err != nil {
		logger.Warn("server reachability probe failed", slog.String("server", target.Name), slog.String("error", err.Error()))
	}
	store.RecordProbe(target.Name, ProbeResult{
		Depth:       depth,
		AttemptedAt: attemptedAt,
		Success:     reachable && err == nil,
		Error:       errorText(err),
	})
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// firstTickDelay deterministically spreads workers over the first interval.
// FNV is stable across restarts and avoids a synchronized burst without a
// shared random source or test-only timing hooks.
func firstTickDelay(name string, interval time.Duration) time.Duration {
	if interval <= time.Nanosecond {
		return 0
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(name))
	return time.Duration(h.Sum64()%uint64(interval-time.Nanosecond)) + time.Nanosecond
}
