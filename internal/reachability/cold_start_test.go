package reachability_test

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/mcp"
	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/reachability"
)

type deadlineProbe struct {
	budgets chan time.Duration
}

func (p *deadlineProbe) Probe(ctx context.Context, _ reachability.Target) reachability.Attempt {
	deadline, _ := ctx.Deadline()
	select {
	case p.budgets <- time.Until(deadline):
	default:
	}
	return reachability.Attempt{Disposition: reachability.DispositionSuccess}
}

func TestProbeBudgetsPreserveListenerAndCallerBounds(t *testing.T) {
	for _, tc := range []struct {
		name          string
		callerTimeout time.Duration
		listenerMax   time.Duration
		deepMax       time.Duration
	}{
		{"independent", time.Minute, 5 * time.Second, 30 * time.Second},
		{"caller-bounded", 200 * time.Millisecond, 200 * time.Millisecond, 200 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			listener := &deadlineProbe{budgets: make(chan time.Duration, 1)}
			deep := &deadlineProbe{budgets: make(chan time.Duration, 1)}
			manager := reachability.NewManager(nil, reachability.NewVersionSelector(listener, deep))
			ctx, cancel := context.WithTimeout(context.Background(), tc.callerTimeout)
			defer cancel()
			if err := manager.Start(ctx, reachability.Target{Name: "budgets"}, time.Millisecond); err != nil {
				t.Fatal(err)
			}
			defer manager.Close()
			for _, expected := range []struct {
				probe *deadlineProbe
				max   time.Duration
			}{{listener, tc.listenerMax}, {deep, tc.deepMax}} {
				select {
				case budget := <-expected.probe.budgets:
					if budget <= expected.max-100*time.Millisecond || budget > expected.max {
						t.Fatalf("probe budget %s, want within 100ms of %s", budget, expected.max)
					}
				case <-ctx.Done():
					t.Fatal("probe did not start before caller deadline")
				}
			}
		})
	}
}

func TestEndToEndProbeAllowsColdStart(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(6 * time.Second):
		}
		w.Header().Set("Mcp-Session-Id", "cold-start-probe")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	store := reachability.NewStore()
	listener := &recordingProbe{disposition: reachability.DispositionSuccess}
	manager := reachability.NewManager(store, reachability.NewVersionSelector(listener, mcp.NewEndToEndProbe()))
	target := reachability.Target{Name: "cold-start", Port: server.Listener.Addr().(*net.TCPAddr).Port}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if err := manager.Start(ctx, target, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	defer manager.Close()

	for ctx.Err() == nil {
		value, _ := store.Get(target.Name)
		if deep, ok := value.Evidence[reachability.DepthEndToEnd]; ok && deep.LastProbeOutcome != "" {
			if deep.LastProbeOutcome != reachability.OutcomeSuccess {
				t.Fatalf("cold-start probe failed: %s", deep.LastProbeError)
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("cold-start probe did not complete within its caller deadline")
}
