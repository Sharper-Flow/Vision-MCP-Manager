package mcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestLeaseSnapshotBoundsClosedHistoryAndNeverExposesRawID(t *testing.T) {
	clock := &fakeLeaseClock{now: time.Unix(1_700_000_000, 0)}
	mgr := NewLeaseManager(0, time.Minute, clock)
	for i := 0; i < maxClosedLeaseHistory+5; i++ {
		reservation, err := mgr.Reserve()
		if err != nil {
			t.Fatal(err)
		}
		id := fmt.Sprintf("raw-private-session-id-%d", i)
		if err := mgr.Commit(reservation, id); err != nil {
			t.Fatal(err)
		}
		if !mgr.TryBeginExpiry(id, "idle_timeout") || !mgr.FinalizeClose(id) {
			t.Fatalf("failed to close %q", id)
		}
	}
	snapshot := mgr.Snapshot(100)
	if len(snapshot.Closed) != maxClosedLeaseHistory || snapshot.ClosedOmitted != 5 {
		t.Fatalf("closed=%d omitted=%d", len(snapshot.Closed), snapshot.ClosedOmitted)
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "raw-private-session-id") {
		t.Fatal("snapshot exposed raw session identifier")
	}
}

func newLeaseManagerForDisconnectTest(t *testing.T, grace time.Duration) (*LeaseManager, *fakeLeaseClock) {
	t.Helper()
	clock := &fakeLeaseClock{now: time.Unix(500, 0)}
	mgr := NewLeaseManagerWithDisconnectGrace(1, time.Hour, grace, clock)
	reservation, err := mgr.Reserve()
	if err != nil {
		t.Fatal(err)
	}
	if err := mgr.Commit(reservation, "session-a"); err != nil {
		t.Fatal(err)
	}
	return mgr, clock
}

func TestLeaseManagerBeginSSEMarksEverStreamedAndClearsDeadline(t *testing.T) {
	mgr, _ := newLeaseManagerForDisconnectTest(t, time.Minute)
	mgr.mu.Lock()
	mgr.leases["session-a"].disconnectDeadline = time.Unix(600, 0)
	mgr.mu.Unlock()

	if err := mgr.BeginSSE("session-a"); err != nil {
		t.Fatal(err)
	}
	mgr.mu.Lock()
	lease := *mgr.leases["session-a"]
	mgr.mu.Unlock()
	if !lease.everStreamed {
		t.Fatal("BeginSSE() did not mark lease as ever streamed")
	}
	if !lease.disconnectDeadline.IsZero() {
		t.Fatalf("BeginSSE() deadline = %v, want zero", lease.disconnectDeadline)
	}
}

func TestLeaseManagerEndSSEToZeroArmsDisconnectDeadline(t *testing.T) {
	grace := 2 * time.Minute
	mgr, clock := newLeaseManagerForDisconnectTest(t, grace)
	if err := mgr.BeginSSE("session-a"); err != nil {
		t.Fatal(err)
	}
	mgr.EndSSE("session-a")

	want := clock.Now().Add(grace)
	mgr.mu.Lock()
	got := mgr.leases["session-a"].disconnectDeadline
	mgr.mu.Unlock()
	if !got.Equal(want) {
		t.Fatalf("EndSSE() deadline = %v, want %v", got, want)
	}
}

func TestLeaseManagerEndSSEWithZeroGraceDoesNotArmDisconnectDeadline(t *testing.T) {
	mgr, _ := newLeaseManagerForDisconnectTest(t, 0)
	if err := mgr.BeginSSE("session-a"); err != nil {
		t.Fatal(err)
	}
	mgr.EndSSE("session-a")

	mgr.mu.Lock()
	got := mgr.leases["session-a"].disconnectDeadline
	mgr.mu.Unlock()
	if !got.IsZero() {
		t.Fatalf("EndSSE() deadline = %v, want zero", got)
	}
}

func TestLeaseManagerApplicationBeginRequestWithZeroDeadlineDoesNotArm(t *testing.T) {
	mgr, _ := newLeaseManagerForDisconnectTest(t, time.Minute)
	done, err := mgr.BeginRequest("session-a", true)
	if err != nil {
		t.Fatal(err)
	}
	mgr.mu.Lock()
	got := mgr.leases["session-a"].disconnectDeadline
	mgr.mu.Unlock()
	if !got.IsZero() {
		t.Fatalf("BeginRequest() deadline = %v, want zero", got)
	}
	done()

	mgr.mu.Lock()
	got = mgr.leases["session-a"].disconnectDeadline
	mgr.mu.Unlock()
	if !got.IsZero() {
		t.Fatalf("completed BeginRequest() deadline = %v, want zero", got)
	}
}

func TestLeaseManagerApplicationBeginRequestRearmsDisconnectDeadline(t *testing.T) {
	grace := 2 * time.Minute
	mgr, clock := newLeaseManagerForDisconnectTest(t, grace)
	oldDeadline := clock.Now().Add(time.Second)
	mgr.mu.Lock()
	mgr.leases["session-a"].disconnectDeadline = oldDeadline
	mgr.mu.Unlock()
	clock.Advance(2 * time.Second)

	done, err := mgr.BeginRequest("session-a", true)
	if err != nil {
		t.Fatal(err)
	}
	want := clock.Now().Add(grace)
	mgr.mu.Lock()
	got := mgr.leases["session-a"].disconnectDeadline
	mgr.mu.Unlock()
	if !got.Equal(want) {
		t.Fatalf("BeginRequest() deadline = %v, want %v", got, want)
	}
	done()
}

func TestLeaseManagerMaintenanceBeginRequestDoesNotRearmDisconnectDeadline(t *testing.T) {
	grace := 2 * time.Minute
	mgr, clock := newLeaseManagerForDisconnectTest(t, grace)
	deadline := clock.Now().Add(time.Minute)
	mgr.mu.Lock()
	mgr.leases["session-a"].disconnectDeadline = deadline
	mgr.mu.Unlock()
	clock.Advance(2 * time.Second)

	done, err := mgr.BeginRequest("session-a", false)
	if err != nil {
		t.Fatal(err)
	}
	done()

	mgr.mu.Lock()
	got := mgr.leases["session-a"].disconnectDeadline
	mgr.mu.Unlock()
	if !got.Equal(deadline) {
		t.Fatalf("maintenance BeginRequest() deadline = %v, want %v", got, deadline)
	}
}

func TestLeaseManagerBeginSSEClearsArmedDisconnectDeadline(t *testing.T) {
	mgr, _ := newLeaseManagerForDisconnectTest(t, time.Minute)
	mgr.mu.Lock()
	mgr.leases["session-a"].disconnectDeadline = time.Unix(600, 0)
	mgr.mu.Unlock()

	if err := mgr.BeginSSE("session-a"); err != nil {
		t.Fatal(err)
	}
	mgr.mu.Lock()
	got := mgr.leases["session-a"].disconnectDeadline
	mgr.mu.Unlock()
	if !got.IsZero() {
		t.Fatalf("BeginSSE() deadline = %v, want zero", got)
	}
}

func TestLeaseManagerEndSSEFromTwoConnectionsDoesNotArmDeadline(t *testing.T) {
	mgr, _ := newLeaseManagerForDisconnectTest(t, time.Minute)
	if err := mgr.BeginSSE("session-a"); err != nil {
		t.Fatal(err)
	}
	if err := mgr.BeginSSE("session-a"); err != nil {
		t.Fatal(err)
	}
	mgr.EndSSE("session-a")

	mgr.mu.Lock()
	lease := *mgr.leases["session-a"]
	mgr.mu.Unlock()
	if lease.sseCount != 1 {
		t.Fatalf("sseCount = %d, want 1", lease.sseCount)
	}
	if !lease.disconnectDeadline.IsZero() {
		t.Fatalf("EndSSE() deadline = %v, want zero", lease.disconnectDeadline)
	}
}

type fakeLeaseClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeLeaseClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeLeaseClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func TestClassifyApplicationActivity(t *testing.T) {
	tests := []struct {
		name string
		body string
		want bool
	}{
		{name: "tool request", body: `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{}}`, want: true},
		{name: "list request", body: `{"jsonrpc":"2.0","id":"a","method":"tools/list"}`, want: true},
		{name: "ping request", body: `{"jsonrpc":"2.0","id":2,"method":"ping"}`, want: false},
		{name: "notification", body: `{"jsonrpc":"2.0","method":"notifications/initialized"}`, want: false},
		{name: "response", body: `{"jsonrpc":"2.0","id":2,"result":{}}`, want: false},
		{name: "mixed batch", body: `[{"jsonrpc":"2.0","id":1,"method":"ping"},{"jsonrpc":"2.0","id":2,"method":"resources/read"}]`, want: true},
		{name: "maintenance batch", body: `[{"jsonrpc":"2.0","id":1,"method":"ping"},{"jsonrpc":"2.0","method":"notifications/cancelled"}]`, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ClassifyApplicationActivity([]byte(tt.body))
			if err != nil {
				t.Fatalf("ClassifyApplicationActivity() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("ClassifyApplicationActivity() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestClassifyApplicationActivityRejectsInvalidEnvelope(t *testing.T) {
	for _, body := range []string{``, `{}`, `[]`, `{"jsonrpc":"2.0","id":1}`, `not-json`} {
		if _, err := ClassifyApplicationActivity([]byte(body)); err == nil {
			t.Fatalf("ClassifyApplicationActivity(%q) succeeded, want error", body)
		}
	}
}

func TestLeaseManagerReservationCapacityAndCommit(t *testing.T) {
	clock := &fakeLeaseClock{now: time.Unix(100, 0)}
	mgr := NewLeaseManager(2, 30*time.Minute, clock)

	r1, err := mgr.Reserve()
	if err != nil {
		t.Fatal(err)
	}
	r2, err := mgr.Reserve()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.Reserve(); !errors.Is(err, ErrLeaseCapacity) {
		t.Fatalf("third Reserve() error = %v, want ErrLeaseCapacity", err)
	}
	if err := mgr.Commit(r1, "session-a"); err != nil {
		t.Fatal(err)
	}
	mgr.ReleaseReservation(r2)

	if got := mgr.CapacityUsed(); got != 1 {
		t.Fatalf("CapacityUsed() = %d, want 1", got)
	}
	if _, err := mgr.Reserve(); err != nil {
		t.Fatalf("Reserve() after release failed: %v", err)
	}
}

func TestLeaseManagerRequestGuardBlocksExpiryAndCompletesOnce(t *testing.T) {
	clock := &fakeLeaseClock{now: time.Unix(200, 0)}
	mgr := NewLeaseManager(1, 30*time.Minute, clock)
	r, _ := mgr.Reserve()
	if err := mgr.Commit(r, "session-a"); err != nil {
		t.Fatal(err)
	}
	mgr.mu.Lock()
	mgr.leases["session-a"].everStreamed = true
	mgr.mu.Unlock()

	clock.Advance(31 * time.Minute)
	done, err := mgr.BeginRequest("session-a", true)
	if err != nil {
		t.Fatal(err)
	}
	if expired := mgr.ExpireEligible(); len(expired) != 0 {
		t.Fatalf("ExpireEligible() = %v while request in flight", expired)
	}

	done()
	done()
	if got := mgr.InFlight("session-a"); got != 0 {
		t.Fatalf("InFlight() = %d, want 0", got)
	}
	clock.Advance(31 * time.Minute)
	if expired := mgr.ExpireEligible(); len(expired) != 1 || expired[0] != (ExpiredLease{SessionID: "session-a", Reason: "idle_timeout"}) {
		t.Fatalf("ExpireEligible() = %v, want [{session-a idle_timeout}]", expired)
	}
	if _, err := mgr.BeginRequest("session-a", true); !errors.Is(err, ErrLeaseNotActive) {
		t.Fatalf("BeginRequest() after expiry error = %v, want ErrLeaseNotActive", err)
	}
}

func TestLeaseManagerMaintenanceTrafficDoesNotRefreshActivity(t *testing.T) {
	clock := &fakeLeaseClock{now: time.Unix(300, 0)}
	mgr := NewLeaseManager(1, 30*time.Minute, clock)
	r, _ := mgr.Reserve()
	if err := mgr.Commit(r, "session-a"); err != nil {
		t.Fatal(err)
	}

	clock.Advance(20 * time.Minute)
	done, err := mgr.BeginRequest("session-a", false)
	if err != nil {
		t.Fatal(err)
	}
	done()
	mgr.BeginSSE("session-a")
	mgr.EndSSE("session-a")
	clock.Advance(11 * time.Minute)

	if expired := mgr.ExpireEligible(); len(expired) != 1 || expired[0].Reason != "idle_timeout" {
		t.Fatalf("ExpireEligible() = %v, want one idle_timeout lease", expired)
	}
}

func TestLeaseManagerExpireEligibleRulesAndPrecedence(t *testing.T) {
	tests := []struct {
		name       string
		idle       time.Duration
		grace      time.Duration
		configure  func(*leaseRecord, time.Time)
		wantReason string
	}{
		{
			name:  "client disconnected",
			idle:  time.Hour,
			grace: time.Minute,
			configure: func(lease *leaseRecord, now time.Time) {
				lease.everStreamed = true
				lease.lastActivity = now
				lease.disconnectDeadline = now
			},
			wantReason: "client_disconnected",
		},
		{
			name:  "never streamed",
			idle:  time.Hour,
			grace: time.Minute,
			configure: func(lease *leaseRecord, now time.Time) {
				lease.lastActivity = now.Add(-5 * time.Minute)
			},
			wantReason: "never_streamed",
		},
		{
			name:  "idle timeout",
			idle:  5 * time.Minute,
			grace: 0,
			configure: func(lease *leaseRecord, now time.Time) {
				lease.everStreamed = true
				lease.lastActivity = now.Add(-5 * time.Minute)
			},
			wantReason: "idle_timeout",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clock := &fakeLeaseClock{now: time.Unix(700, 0)}
			mgr := NewLeaseManagerWithDisconnectGrace(1, tt.idle, tt.grace, clock)
			reservation, err := mgr.Reserve()
			if err != nil {
				t.Fatal(err)
			}
			if err := mgr.Commit(reservation, "session-a"); err != nil {
				t.Fatal(err)
			}
			mgr.mu.Lock()
			tt.configure(mgr.leases["session-a"], clock.Now())
			mgr.mu.Unlock()

			got := mgr.ExpireEligible()
			want := []ExpiredLease{{SessionID: "session-a", Reason: tt.wantReason}}
			if len(got) != len(want) || got[0] != want[0] {
				t.Fatalf("ExpireEligible() = %v, want %v", got, want)
			}
		})
	}
}

func TestLeaseManagerExpireEligiblePrecedence(t *testing.T) {
	tests := []struct {
		name       string
		idle       time.Duration
		grace      time.Duration
		configure  func(*leaseRecord, time.Time)
		wantReason string
	}{
		{
			name:  "disconnect precedes idle",
			idle:  time.Minute,
			grace: time.Minute,
			configure: func(lease *leaseRecord, now time.Time) {
				lease.everStreamed = true
				lease.lastActivity = now.Add(-time.Minute)
				lease.disconnectDeadline = now.Add(-time.Second)
			},
			wantReason: "client_disconnected",
		},
		{
			name:  "never streamed precedes idle",
			idle:  10 * time.Minute,
			grace: time.Minute,
			configure: func(lease *leaseRecord, now time.Time) {
				lease.lastActivity = now.Add(-10 * time.Minute)
			},
			wantReason: "never_streamed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clock := &fakeLeaseClock{now: time.Unix(800, 0)}
			mgr := NewLeaseManagerWithDisconnectGrace(1, tt.idle, tt.grace, clock)
			reservation, err := mgr.Reserve()
			if err != nil {
				t.Fatal(err)
			}
			if err := mgr.Commit(reservation, "session-a"); err != nil {
				t.Fatal(err)
			}
			mgr.mu.Lock()
			tt.configure(mgr.leases["session-a"], clock.Now())
			mgr.mu.Unlock()

			got := mgr.ExpireEligible()
			if len(got) != 1 || got[0].Reason != tt.wantReason {
				t.Fatalf("ExpireEligible() = %v, want reason %q", got, tt.wantReason)
			}
		})
	}
}

func TestLeaseManagerExpireEligibleNeverSweepsInflight(t *testing.T) {
	clock := &fakeLeaseClock{now: time.Unix(900, 0)}
	mgr := NewLeaseManagerWithDisconnectGrace(3, time.Minute, time.Minute, clock)
	for _, id := range []string{"disconnect", "never", "idle"} {
		reservation, err := mgr.Reserve()
		if err != nil {
			t.Fatal(err)
		}
		if err := mgr.Commit(reservation, id); err != nil {
			t.Fatal(err)
		}
	}
	mgr.mu.Lock()
	mgr.leases["disconnect"].disconnectDeadline = clock.Now().Add(-time.Second)
	mgr.leases["disconnect"].everStreamed = true
	mgr.leases["never"].lastActivity = clock.Now().Add(-5 * time.Minute)
	mgr.leases["idle"].everStreamed = true
	mgr.leases["idle"].lastActivity = clock.Now().Add(-time.Minute)
	for _, lease := range mgr.leases {
		lease.inFlight = 1
	}
	mgr.mu.Unlock()

	if got := mgr.ExpireEligible(); len(got) != 0 {
		t.Fatalf("ExpireEligible() = %v with all rules elapsed in-flight", got)
	}
}

func TestLeaseManagerExpireEligibleNeverStreamedRequiresNoStream(t *testing.T) {
	clock := &fakeLeaseClock{now: time.Unix(1000, 0)}
	mgr := NewLeaseManagerWithDisconnectGrace(1, 10*time.Minute, time.Minute, clock)
	reservation, err := mgr.Reserve()
	if err != nil {
		t.Fatal(err)
	}
	if err := mgr.Commit(reservation, "session-a"); err != nil {
		t.Fatal(err)
	}
	if err := mgr.BeginSSE("session-a"); err != nil {
		t.Fatal(err)
	}
	mgr.EndSSE("session-a")
	mgr.mu.Lock()
	mgr.leases["session-a"].disconnectDeadline = time.Time{}
	mgr.mu.Unlock()
	clock.Advance(6 * time.Minute)

	if got := mgr.ExpireEligible(); len(got) != 0 {
		t.Fatalf("ExpireEligible() = %v after streaming, want no never_streamed expiry", got)
	}
}

func TestLeaseManagerExpireEligibleIdleDisabledKeepsOtherRules(t *testing.T) {
	clock := &fakeLeaseClock{now: time.Unix(1100, 0)}
	mgr := NewLeaseManagerWithDisconnectGrace(3, 0, time.Minute, clock)
	for _, id := range []string{"disconnect", "never", "streamed"} {
		reservation, err := mgr.Reserve()
		if err != nil {
			t.Fatal(err)
		}
		if err := mgr.Commit(reservation, id); err != nil {
			t.Fatal(err)
		}
	}
	mgr.mu.Lock()
	mgr.leases["disconnect"].disconnectDeadline = clock.Now().Add(-time.Second)
	mgr.leases["disconnect"].everStreamed = true
	// grace 1m -> never-streamed bound is 5m even though idleTimeout is disabled.
	mgr.leases["never"].lastActivity = clock.Now().Add(-6 * time.Minute)
	mgr.leases["streamed"].everStreamed = true
	mgr.leases["streamed"].lastActivity = clock.Now().Add(-time.Hour)
	mgr.mu.Unlock()

	got := mgr.ExpireEligible()
	if len(got) != 2 || got[0] != (ExpiredLease{SessionID: "disconnect", Reason: "client_disconnected"}) || got[1] != (ExpiredLease{SessionID: "never", Reason: "never_streamed"}) {
		t.Fatalf("ExpireEligible() = %v, want disconnect and never_streamed only", got)
	}
}

// A never-streamed lease must not be reaped the instant it is created. When
// idleTimeout is 0 the never-streamed bound must still derive from the grace
// period, not collapse to 0 and make every rule-2 comparison trivially true.
func TestLeaseManagerExpireEligibleFreshNeverStreamedLeaseSurvives(t *testing.T) {
	clock := &fakeLeaseClock{now: time.Unix(1300, 0)}
	mgr := NewLeaseManagerWithDisconnectGrace(3, 0, time.Minute, clock)
	reservation, err := mgr.Reserve()
	if err != nil {
		t.Fatal(err)
	}
	if err := mgr.Commit(reservation, "fresh"); err != nil {
		t.Fatal(err)
	}

	if got := mgr.ExpireEligible(); len(got) != 0 {
		t.Fatalf("ExpireEligible() = %v, want no expiry for a lease created this instant", got)
	}

	// 5 * grace = 5m is the derived bound; just under it must still survive.
	clock.Advance(4*time.Minute + 59*time.Second)
	if got := mgr.ExpireEligible(); len(got) != 0 {
		t.Fatalf("ExpireEligible() = %v, want no expiry before the never-streamed bound", got)
	}

	clock.Advance(2 * time.Second)
	got := mgr.ExpireEligible()
	if len(got) != 1 || got[0] != (ExpiredLease{SessionID: "fresh", Reason: "never_streamed"}) {
		t.Fatalf("ExpireEligible() = %v, want never_streamed after the bound", got)
	}
}

// With both idleTimeout and grace disabled there is no derived bound, so
// rule 2 must not fire at all.
func TestLeaseManagerExpireEligibleNeverStreamedDisabledWhenNoBound(t *testing.T) {
	clock := &fakeLeaseClock{now: time.Unix(1400, 0)}
	mgr := NewLeaseManagerWithDisconnectGrace(3, 0, 0, clock)
	reservation, err := mgr.Reserve()
	if err != nil {
		t.Fatal(err)
	}
	if err := mgr.Commit(reservation, "fresh"); err != nil {
		t.Fatal(err)
	}

	clock.Advance(24 * time.Hour)
	if got := mgr.ExpireEligible(); len(got) != 0 {
		t.Fatalf("ExpireEligible() = %v, want no expiry when every bound is disabled", got)
	}
}

func TestLeaseManagerExpireEligibleSortsSessionIDs(t *testing.T) {
	clock := &fakeLeaseClock{now: time.Unix(1200, 0)}
	mgr := NewLeaseManager(3, time.Minute, clock)
	for _, id := range []string{"z-session", "a-session", "m-session"} {
		reservation, err := mgr.Reserve()
		if err != nil {
			t.Fatal(err)
		}
		if err := mgr.Commit(reservation, id); err != nil {
			t.Fatal(err)
		}
	}
	mgr.mu.Lock()
	for _, lease := range mgr.leases {
		lease.everStreamed = true
		lease.lastActivity = clock.Now().Add(-time.Minute)
	}
	mgr.mu.Unlock()

	got := mgr.ExpireEligible()
	want := []ExpiredLease{
		{SessionID: "a-session", Reason: "idle_timeout"},
		{SessionID: "m-session", Reason: "idle_timeout"},
		{SessionID: "z-session", Reason: "idle_timeout"},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("ExpireEligible() = %v, want sorted %v", got, want)
	}
}

func TestLeaseManagerExpireEligibleSkipsNonActiveLeases(t *testing.T) {
	clock := &fakeLeaseClock{now: time.Unix(1300, 0)}
	mgr := NewLeaseManagerWithDisconnectGrace(2, time.Minute, time.Minute, clock)
	for _, id := range []string{"expiring", "uncertain"} {
		reservation, err := mgr.Reserve()
		if err != nil {
			t.Fatal(err)
		}
		if err := mgr.Commit(reservation, id); err != nil {
			t.Fatal(err)
		}
	}
	mgr.mu.Lock()
	mgr.leases["expiring"].state = LeaseStateExpiring
	mgr.leases["expiring"].disconnectDeadline = clock.Now().Add(-time.Second)
	mgr.leases["uncertain"].state = LeaseStateCleanupUncertain
	mgr.leases["uncertain"].lastActivity = clock.Now().Add(-time.Hour)
	mgr.mu.Unlock()

	if got := mgr.ExpireEligible(); len(got) != 0 {
		t.Fatalf("ExpireEligible() = %v for non-active leases", got)
	}
}

func TestLeaseManagerConcurrentReservationNeverExceedsLimit(t *testing.T) {
	clock := &fakeLeaseClock{now: time.Unix(400, 0)}
	mgr := NewLeaseManager(6, 30*time.Minute, clock)

	var wg sync.WaitGroup
	var successes int
	var mu sync.Mutex
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := mgr.Reserve(); err == nil {
				mu.Lock()
				successes++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if successes != 6 {
		t.Fatalf("successful reservations = %d, want 6", successes)
	}
	if got := mgr.CapacityUsed(); got != 6 {
		t.Fatalf("CapacityUsed() = %d, want 6", got)
	}
}
