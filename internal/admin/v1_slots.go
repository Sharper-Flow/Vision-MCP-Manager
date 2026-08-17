package admin

import (
	"cmp"
	"encoding/json"
	"net/http"
	"slices"
	"time"

	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/config"
	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/server"
)

// handleV1Slots handles GET /v1/slots — returns a JSON envelope with all
// slot groups and their per-slot session detail. Mirrors the data model
// from the vision_slot_status MCP tool but exposed as a plain HTTP endpoint.
//
// Public endpoint (no SecurityMiddleware). Admin server is localhost-bound
// by default; status visibility is considered low-risk.
func (s *Server) handleV1Slots(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	running := s.running
	s.mu.RUnlock()

	if !running {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "unhealthy"})
		return
	}

	groups := s.buildSlotGroupStatuses()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"groups": groups})
}

// handleV1SlotsGroup handles GET /v1/slots/{group} — returns a single
// group's slot detail, or 404 if the group name is not configured.
func (s *Server) handleV1SlotsGroup(w http.ResponseWriter, r *http.Request) {
	group := r.PathValue("group")
	if group == "" {
		http.Error(w, "missing group name", http.StatusBadRequest)
		return
	}

	s.mu.RLock()
	running := s.running
	s.mu.RUnlock()

	if !running {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "unhealthy"})
		return
	}

	for _, g := range s.buildSlotGroupStatuses() {
		if g.GroupName == group {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(g)
			return
		}
	}

	http.Error(w, "slot group not found", http.StatusNotFound)
}

// buildSlotGroupStatuses reuses the same data model as toolSlotStatus
// (SlotGroupStatus / SlotDetail) to produce a sorted slice of group
// statuses. Extracted as a shared helper so both the MCP tool and HTTP
// handler use identical logic.
func (s *Server) buildSlotGroupStatuses() []SlotGroupStatus {
	if s.daemonConfig == nil || len(s.daemonConfig.SlotGroups) == 0 {
		return []SlotGroupStatus{}
	}

	groups := make([]SlotGroupStatus, 0, len(s.daemonConfig.SlotGroups))
	for groupName := range s.daemonConfig.SlotGroups {
		g := SlotGroupStatus{GroupName: groupName}

		for srvName, srvCfg := range s.daemonConfig.Servers {
			if srvCfg == nil || srvCfg.SlotGroup != groupName {
				continue
			}
			g.Slots = append(g.Slots, s.slotDetail(srvName, srvCfg))
		}

		// Sort slots by name for deterministic output.
		slices.SortFunc(g.Slots, func(a, b SlotDetail) int {
			return cmp.Compare(a.Name, b.Name)
		})

		g.SlotCount = len(g.Slots)
		groups = append(groups, g)
	}

	slices.SortFunc(groups, func(a, b SlotGroupStatus) int {
		return cmp.Compare(a.GroupName, b.GroupName)
	})
	return groups
}

// slotDetail projects the config, session, lifecycle, and reachability state
// for one slot. The effective status uses the same precedence table as the
// other admin surfaces, so an unreachable slot cannot appear healthy here.
func (s *Server) slotDetail(name string, cfg *config.ServerConfig) SlotDetail {
	slot := SlotDetail{
		Name:        name,
		Port:        cfg.Port,
		MaxSessions: cfg.MaxSessions,
	}
	if s.slotSessionAccessor != nil {
		slot.ActiveSessions = s.slotSessionAccessor.ActiveSessionCount(name)
	}

	processState := server.StateStopped
	processUptime := time.Duration(0)
	var rawReason string
	if s.registry != nil {
		if managed := s.registry.Get(name); managed != nil {
			status := managed.Status()
			processState = status.State
			processUptime = status.Uptime
			rawReason = status.LastError
		}
	}
	lifecycle := s.lifecycleSnapshot(name)
	effective := deriveEffectiveStatus(
		processState,
		lifecycleBackendState(lifecycle),
		s.reachabilityFor(name),
		processUptime,
		s.reachabilityGrace,
		rawReason,
	)
	slot.EffectiveStatus = effective.Status
	slot.ReachabilityDetails = effective.Reachability
	if effective.Reason != "" {
		reason := effective.Reason
		slot.EffectiveReason = &reason
	}
	return slot
}
