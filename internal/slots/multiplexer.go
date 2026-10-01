package slots

import (
	"context"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/metrics"
	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/session"
)

type Entry struct {
	SlotName string
	Index    int
	Manager  *session.Manager
	Healthy  func() bool

	// Reporter owns the member server's session gauge. Sessions the group
	// routes to this member increment this reporter so read-time daemon
	// derivation counts them.
	Reporter metrics.ServerMetricsReporter
}

type slotEntry struct {
	slotName         string
	index            int
	mgr              *session.Manager
	reporter         metrics.ServerMetricsReporter
	pending          int
	healthy          func() bool
	quarantinedUntil time.Time
}

type Multiplexer struct {
	groupName  string
	slots      []*slotEntry
	byUpstream map[string]*slotEntry
	logger     *slog.Logger
	mu         sync.RWMutex

	// onSessionRemoved is the group proxy's removal observer. It is added to
	// each member manager as an extra observer so the member endpoint's own
	// removal callback survives, and it is re-applied to replacement member
	// managers in ReplaceEntries. hooked records which managers already carry
	// it so re-registration never stacks duplicates.
	onSessionRemoved func(sessionID string)
	hooked           map[*session.Manager]bool
}

func NewMultiplexer(groupName string, logger *slog.Logger, entries []Entry) *Multiplexer {
	if logger == nil {
		logger = slog.Default()
	}
	slots := make([]*slotEntry, 0, len(entries))
	for _, entry := range entries {
		slots = append(slots, &slotEntry{slotName: entry.SlotName, index: entry.Index, mgr: entry.Manager, reporter: entry.Reporter, healthy: entry.Healthy})
	}
	sort.Slice(slots, func(i, j int) bool {
		return slots[i].index < slots[j].index
	})
	return &Multiplexer{
		groupName:  groupName,
		slots:      slots,
		byUpstream: make(map[string]*slotEntry),
		logger:     logger,
	}
}

func (m *Multiplexer) ReplaceEntries(entries []Entry) {
	m.mu.Lock()
	updated := make([]*slotEntry, 0, len(entries))
	for _, entry := range entries {
		var existing *slotEntry
		for _, slot := range m.slots {
			if slot.slotName == entry.SlotName {
				existing = slot
				break
			}
		}
		if existing != nil {
			existing.index = entry.Index
			existing.mgr = entry.Manager
			existing.reporter = entry.Reporter
			existing.healthy = entry.Healthy
			updated = append(updated, existing)
			continue
		}
		updated = append(updated, &slotEntry{slotName: entry.SlotName, index: entry.Index, mgr: entry.Manager, reporter: entry.Reporter, healthy: entry.Healthy})
	}
	sort.Slice(updated, func(i, j int) bool { return updated[i].index < updated[j].index })
	m.slots = updated
	// Snapshot the manager pointers while still holding the lock: the hook
	// pass works on the snapshot, so a concurrent ReplaceEntries writing the
	// same slot entry can never race a read of entry.mgr outside the lock.
	mgrs := make([]*session.Manager, 0, len(updated))
	for _, entry := range updated {
		mgrs = append(mgrs, entry.mgr)
	}
	m.mu.Unlock()

	// Replacement member managers must observe group-session removals too,
	// or a reap on a restarted member strands the session gauge.
	for _, mgr := range mgrs {
		m.applyRemovalHook(mgr)
	}
}

// ReporterFor resolves the per-server metrics reporter owning the given
// member manager's sessions, so the proxy can feed group-created sessions
// into their existing owner.
func (m *Multiplexer) ReporterFor(mgr *session.Manager) metrics.ServerMetricsReporter {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, slot := range m.slots {
		if slot != nil && slot.mgr == mgr {
			return slot.reporter
		}
	}
	return nil
}

func (m *Multiplexer) SelectForNewSession(_ context.Context, upstreamSessionID string) (*session.Manager, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if existing, ok := m.byUpstream[upstreamSessionID]; ok {
		return existing.mgr, nil
	}

	var chosen *slotEntry
	for _, slot := range m.slots {
		if !slotHealthy(slot) {
			m.logger.Info("skipping unhealthy slot",
				slog.String("event", "slot.health_skipped"),
				slog.String("group", m.groupName),
				slog.String("slot_name", slot.slotName),
				slog.Int("slot_index", slot.index),
				slog.String("upstream_session_id", upstreamSessionID),
			)
			continue
		}
		if slotAtCapacity(slot) {
			continue
		}
		if chosen == nil || slotLoad(slot) < slotLoad(chosen) || (slotLoad(slot) == slotLoad(chosen) && slot.index < chosen.index) {
			chosen = slot
		}
	}
	if chosen == nil {
		m.logger.Warn("no routable slot available",
			slog.String("event", "slot.routing_full"),
			slog.String("group", m.groupName),
			slog.String("upstream_session_id", upstreamSessionID),
		)
		return nil, session.ErrMaxSessions
	}

	chosen.pending++
	m.byUpstream[upstreamSessionID] = chosen
	m.logger.Info("assigned upstream session to slot",
		slog.String("event", "slot.assigned"),
		slog.String("group", m.groupName),
		slog.String("slot_name", chosen.slotName),
		slog.Int("slot_index", chosen.index),
		slog.String("upstream_session_id", upstreamSessionID),
	)
	return chosen.mgr, nil
}

func (m *Multiplexer) Release(upstreamSessionID string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	entry, ok := m.byUpstream[upstreamSessionID]
	if !ok {
		return
	}
	delete(m.byUpstream, upstreamSessionID)
	if entry.pending > 0 {
		entry.pending--
	}
	m.logger.Info("released upstream session slot binding",
		slog.String("event", "slot.released"),
		slog.String("group", m.groupName),
		slog.String("slot_name", entry.slotName),
		slog.Int("slot_index", entry.index),
		slog.String("upstream_session_id", upstreamSessionID),
	)
}

func (m *Multiplexer) Rebind(oldKey, newKey string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	entry, ok := m.byUpstream[oldKey]
	if !ok {
		return
	}
	delete(m.byUpstream, oldKey)
	m.byUpstream[newKey] = entry
}

func (m *Multiplexer) ReportSpawnResult(sessionKey string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	entry, ok := m.byUpstream[sessionKey]
	if !ok || entry == nil {
		return
	}
	if err == nil {
		entry.quarantinedUntil = time.Time{}
		return
	}
	entry.quarantinedUntil = time.Now().Add(2 * time.Second)
}

// SetOnSessionRemoved stores the group proxy's removal observer and adds it
// to every current member manager. Member managers keep their own endpoint's
// callback: sessions created through either endpoint must reach their owning
// proxy's closeDownstream.
func (m *Multiplexer) SetOnSessionRemoved(fn func(sessionID string)) {
	m.mu.Lock()
	m.onSessionRemoved = fn
	// Snapshot the current managers under the same lock that published them,
	// so the hook pass reads snapshotted values instead of live slot entries.
	mgrs := make([]*session.Manager, 0, len(m.slots))
	for _, entry := range m.slots {
		mgrs = append(mgrs, entry.mgr)
	}
	m.mu.Unlock()

	for _, mgr := range mgrs {
		m.applyRemovalHook(mgr)
	}
}

// applyRemovalHook adds the group's removal observer to one member manager at
// most once, so repeated entry replacement never stacks duplicate callbacks.
// Callers pass the manager snapshotted under m.mu, so hooking never reads a
// slot entry's manager field outside the lock.
func (m *Multiplexer) applyRemovalHook(mgr *session.Manager) {
	if mgr == nil {
		return
	}
	m.mu.Lock()
	fn := m.onSessionRemoved
	alreadyHooked := m.hooked[mgr]
	if fn != nil && !alreadyHooked {
		if m.hooked == nil {
			m.hooked = make(map[*session.Manager]bool)
		}
		m.hooked[mgr] = true
	}
	m.mu.Unlock()
	if fn != nil && !alreadyHooked {
		mgr.AddOnSessionRemoved(fn)
	}
}

func (m *Multiplexer) CloseAll() {
	m.mu.RLock()
	mgrs := make([]*session.Manager, 0, len(m.slots))
	for _, entry := range m.slots {
		if entry != nil && entry.mgr != nil {
			mgrs = append(mgrs, entry.mgr)
		}
	}
	m.mu.RUnlock()

	for _, mgr := range mgrs {
		mgr.CloseAll()
	}
}

func (m *Multiplexer) AdmissionStatus() (atCapacity bool, current int, max int) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	hasUnlimited := false
	allBoundedAtCapacity := len(m.slots) > 0

	for _, entry := range m.slots {
		if entry == nil {
			continue
		}
		load := slotLoad(entry)
		current += load

		if entry.mgr == nil {
			allBoundedAtCapacity = false
			continue
		}

		_, _, slotMax := entry.mgr.AdmissionStatus()
		if slotMax == 0 {
			hasUnlimited = true
			allBoundedAtCapacity = false
			continue
		}
		max += slotMax
		if load < slotMax {
			allBoundedAtCapacity = false
		}
	}

	if hasUnlimited {
		return false, current, 0
	}
	return allBoundedAtCapacity, current, max
}

func slotLoad(entry *slotEntry) int {
	if entry == nil {
		return 0
	}
	load := entry.pending
	if entry.mgr != nil {
		load += entry.mgr.SessionCount()
	}
	return load
}

func slotAtCapacity(entry *slotEntry) bool {
	if entry == nil || entry.mgr == nil {
		return false
	}
	_, _, max := entry.mgr.AdmissionStatus()
	if max == 0 {
		return false
	}
	return slotLoad(entry) >= max
}

func slotHealthy(entry *slotEntry) bool {
	if entry == nil {
		return false
	}
	if entry.healthy != nil && !entry.healthy() {
		return false
	}
	if !entry.quarantinedUntil.IsZero() && time.Now().Before(entry.quarantinedUntil) {
		return false
	}
	return true
}
