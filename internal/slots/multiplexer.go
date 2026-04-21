package slots

import (
	"context"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/jrede/vision/internal/session"
)

type Entry struct {
	SlotName string
	Index    int
	Manager  *session.Manager
	Healthy  func() bool
}

type slotEntry struct {
	slotName string
	index    int
	mgr      *session.Manager
	pending  int
	healthy  func() bool
	quarantinedUntil time.Time
}

type Multiplexer struct {
	groupName  string
	slots      []*slotEntry
	byUpstream map[string]*slotEntry
	logger     *slog.Logger
	mu         sync.RWMutex
}

func NewMultiplexer(groupName string, logger *slog.Logger, entries []Entry) *Multiplexer {
	if logger == nil {
		logger = slog.Default()
	}
	slots := make([]*slotEntry, 0, len(entries))
	for _, entry := range entries {
		slots = append(slots, &slotEntry{slotName: entry.SlotName, index: entry.Index, mgr: entry.Manager, healthy: entry.Healthy})
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
	defer m.mu.Unlock()

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
			existing.healthy = entry.Healthy
			updated = append(updated, existing)
			continue
		}
		updated = append(updated, &slotEntry{slotName: entry.SlotName, index: entry.Index, mgr: entry.Manager, healthy: entry.Healthy})
	}
	sort.Slice(updated, func(i, j int) bool { return updated[i].index < updated[j].index })
	m.slots = updated
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

func (m *Multiplexer) SetOnSessionRemoved(fn func(sessionID string)) {
	m.mu.RLock()
	entries := make([]*slotEntry, 0, len(m.slots))
	entries = append(entries, m.slots...)
	m.mu.RUnlock()

	for _, entry := range entries {
		if entry != nil && entry.mgr != nil {
			entry.mgr.SetOnSessionRemoved(fn)
		}
	}
}

func (m *Multiplexer) CloseAll() {
	m.mu.RLock()
	entries := make([]*slotEntry, 0, len(m.slots))
	entries = append(entries, m.slots...)
	m.mu.RUnlock()

	for _, entry := range entries {
		if entry != nil && entry.mgr != nil {
			entry.mgr.CloseAll()
		}
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
