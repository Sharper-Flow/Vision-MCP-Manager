package reachability_test

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/mcp"
	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/reachability"
)

type recordingProbe struct {
	mu     sync.Mutex
	result bool
	starts []time.Time
}

func (p *recordingProbe) Probe(_ context.Context, _ reachability.Target) (bool, error) {
	p.mu.Lock()
	p.starts = append(p.starts, time.Now())
	p.mu.Unlock()
	return p.result, nil
}

func (p *recordingProbe) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.starts)
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition was not met")
}

func TestListenerProbeRecordsReachable(t *testing.T) {
	server := httptest.NewServer(mcp.ListenerProbeMiddleware()(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("listener probe reached the client handler")
	})))
	defer server.Close()

	store := reachability.NewStore()
	manager := reachability.NewManager(store, reachability.NewVersionSelector(mcp.NewListenerProbe()))
	port := server.Listener.Addr().(*net.TCPAddr).Port
	manager.Start(context.Background(), reachability.Target{Name: "alive", Port: port, ProtocolVersion: reachability.ProtocolVersion2025_11_25}, 5*time.Millisecond)
	defer manager.Close()

	waitFor(t, func() bool {
		value, ok := store.Get("alive")
		return ok && value.State == reachability.StateReachable
	})
}

func TestProbeFailuresUseStoreThreshold(t *testing.T) {
	store := reachability.NewStore()
	probe := &recordingProbe{}
	manager := reachability.NewManager(store, reachability.NewVersionSelector(probe))
	manager.StartWithProbe(context.Background(), reachability.Target{Name: "dead", Port: 1, ProtocolVersion: reachability.ProtocolVersion2025_11_25}, 5*time.Millisecond, probe)
	defer manager.Close()

	waitFor(t, func() bool {
		value, ok := store.Get("dead")
		return ok && value.State == reachability.StateUnreachable && value.Evidence[reachability.DepthListener].ConsecutiveFailures >= reachability.FailureThreshold
	})
	if probe.count() < reachability.FailureThreshold {
		t.Fatalf("probe attempts=%d, want at least %d", probe.count(), reachability.FailureThreshold)
	}
}

func TestManagerRemoveStopsWorkerAndRemovesEvidence(t *testing.T) {
	store := reachability.NewStore()
	probe := &recordingProbe{result: true}
	manager := reachability.NewManager(store, reachability.NewVersionSelector(probe))
	manager.StartWithProbe(context.Background(), reachability.Target{Name: "removed", Port: 1, ProtocolVersion: reachability.ProtocolVersion2025_11_25}, time.Millisecond, probe)
	waitFor(t, func() bool { return probe.count() > 0 })
	if err := manager.Remove("removed"); err != nil {
		t.Fatal(err)
	}
	count := probe.count()
	time.Sleep(10 * time.Millisecond)
	if got := probe.count(); got != count {
		t.Fatalf("worker continued after removal: before=%d after=%d", count, got)
	}
	if _, ok := store.Get("removed"); ok {
		t.Fatal("store evidence survived removal")
	}
}

func TestFirstTickJitterDiffersByServer(t *testing.T) {
	store := reachability.NewStore()
	first := &recordingProbe{result: true}
	second := &recordingProbe{result: true}
	manager := reachability.NewManager(store, reachability.NewVersionSelector(first))
	manager.StartWithProbe(context.Background(), reachability.Target{Name: "jitter-one", Port: 1, ProtocolVersion: reachability.ProtocolVersion2025_11_25}, 100*time.Millisecond, first)
	manager.StartWithProbe(context.Background(), reachability.Target{Name: "jitter-two", Port: 1, ProtocolVersion: reachability.ProtocolVersion2025_11_25}, 100*time.Millisecond, second)
	defer manager.Close()
	waitFor(t, func() bool { return first.count() > 0 && second.count() > 0 })
	first.mu.Lock()
	firstAt := first.starts[0]
	first.mu.Unlock()
	second.mu.Lock()
	secondAt := second.starts[0]
	second.mu.Unlock()
	if delta := firstAt.Sub(secondAt); delta < 5*time.Millisecond && delta > -5*time.Millisecond {
		t.Fatalf("first ticks were synchronized: delta=%s", delta)
	}
}

func TestVersionSelectorChoosesListenerProbeForSupportedRevisions(t *testing.T) {
	selector := reachability.NewVersionSelector(mcp.NewListenerProbe())
	for _, version := range []string{reachability.ProtocolVersion2024_11_05, reachability.ProtocolVersion2025_03_26, reachability.ProtocolVersion2025_06_18, reachability.ProtocolVersion2025_11_25} {
		probe, err := selector.Select(version)
		if err != nil {
			t.Fatalf("version %s: %v", version, err)
		}
		if _, ok := probe.(*mcp.ListenerProbe); !ok {
			t.Fatalf("version %s selected %T, want *ListenerProbe", version, probe)
		}
	}
	if _, err := selector.Select("2026-07-28"); err == nil {
		t.Fatal("future revision unexpectedly selected current probe")
	}
}

func TestManagerSkipsEndToEndProbeWhenSessionEvidenceIsRecent(t *testing.T) {
	store := reachability.NewStore()
	store.RecordProbe("busy", reachability.ProbeResult{Depth: reachability.DepthSession, Success: true})
	listener := &recordingProbe{result: true}
	deep := &recordingProbe{result: true}
	manager := reachability.NewManager(store, reachability.NewVersionSelector(listener, deep))
	if err := manager.Start(context.Background(), reachability.Target{Name: "busy", Port: 1, ProtocolVersion: reachability.ProtocolVersion2025_11_25}, 5*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	waitFor(t, func() bool { return listener.count() >= 2 })
	store.RecordProbe("busy", reachability.ProbeResult{Depth: reachability.DepthSession, Success: true})
	time.Sleep(10 * time.Millisecond)
	if got := deep.count(); got != 0 {
		t.Fatalf("end-to-end probes with recent session evidence = %d, want 0", got)
	}
}

func TestManagerRunsEndToEndProbeAtLowCadenceWithoutSessionEvidence(t *testing.T) {
	store := reachability.NewStore()
	listener := &recordingProbe{result: true}
	deep := &recordingProbe{result: true}
	manager := reachability.NewManager(store, reachability.NewVersionSelector(listener, deep))
	if err := manager.Start(context.Background(), reachability.Target{Name: "idle", Port: 1, ProtocolVersion: reachability.ProtocolVersion2025_11_25}, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	waitFor(t, func() bool { return deep.count() >= 1 })
	if listener.count() <= deep.count() {
		t.Fatalf("listener probes=%d, end-to-end probes=%d; want deep probe at lower cadence", listener.count(), deep.count())
	}
}

func TestManagerScalesEndToEndCadenceFromConfiguredInterval(t *testing.T) {
	store := reachability.NewStore()
	listener := &recordingProbe{result: true}
	deep := &recordingProbe{result: true}
	manager := reachability.NewManager(store, reachability.NewVersionSelector(listener, deep))
	interval := 20 * time.Millisecond
	if err := manager.Start(context.Background(), reachability.Target{Name: "configured-cadence", Port: 1, ProtocolVersion: reachability.ProtocolVersion2025_11_25}, interval); err != nil {
		t.Fatal(err)
	}
	defer manager.Close()

	waitFor(t, func() bool { return deep.count() >= 1 })
	listener.mu.Lock()
	firstListener := listener.starts[0]
	listener.mu.Unlock()
	deep.mu.Lock()
	firstDeep := deep.starts[0]
	deep.mu.Unlock()
	minimum := interval * reachability.EndToEndProbeIntervalMultiple
	if elapsed := firstDeep.Sub(firstListener); elapsed < minimum-interval {
		t.Fatalf("end-to-end probe started after %s, want at least about %s", elapsed, minimum)
	}
}

func TestManagerShutdownOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	store := reachability.NewStore()
	probe := &recordingProbe{result: true}
	manager := reachability.NewManager(store, reachability.NewVersionSelector(probe))
	manager.StartWithProbe(ctx, reachability.Target{Name: "cancelled", Port: 1, ProtocolVersion: reachability.ProtocolVersion2025_11_25}, time.Millisecond, probe)
	waitFor(t, func() bool { return probe.count() > 0 })

	cancel()
	manager.Wait()

	// Snapshot only after joining. Sampling between cancel() and Wait() races
	// against a probe that was already in flight when cancellation arrived:
	// that probe legitimately completes and increments the counter, which says
	// nothing about whether the worker kept running.
	//
	// Asserting quiescence after the join is the stronger property anyway —
	// once shutdown has completed, no further probe may be issued.
	count := probe.count()
	time.Sleep(20 * time.Millisecond)
	if got := probe.count(); got != count {
		t.Fatalf("worker continued after shutdown completed: before=%d after=%d", count, got)
	}
}

func TestManagerConcurrentStartsLeaveOneWorker(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	store := reachability.NewStore()
	probe := &recordingProbe{result: true}
	manager := reachability.NewManager(store, reachability.NewVersionSelector(probe))
	defer manager.Close()

	const starts = 32
	ready := make(chan struct{})
	var workers sync.WaitGroup
	for range starts {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-ready
			if err := manager.StartWithProbe(ctx, reachability.Target{Name: "same-server", Port: 1}, time.Nanosecond, probe); err != nil {
				t.Errorf("start worker: %v", err)
			}
		}()
	}
	close(ready)
	workers.Wait()
	manager.Close()

	count := probe.count()
	time.Sleep(5 * time.Millisecond)
	if got := probe.count(); got != count {
		t.Fatalf("a superseded worker continued after Close: before=%d after=%d", count, got)
	}
}
