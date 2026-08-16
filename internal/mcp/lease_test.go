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

	clock.Advance(31 * time.Minute)
	done, err := mgr.BeginRequest("session-a", true)
	if err != nil {
		t.Fatal(err)
	}
	if expired := mgr.ExpireIdle(); len(expired) != 0 {
		t.Fatalf("ExpireIdle() = %v while request in flight", expired)
	}

	done()
	done()
	if got := mgr.InFlight("session-a"); got != 0 {
		t.Fatalf("InFlight() = %d, want 0", got)
	}
	clock.Advance(31 * time.Minute)
	if expired := mgr.ExpireIdle(); len(expired) != 1 || expired[0] != "session-a" {
		t.Fatalf("ExpireIdle() = %v, want [session-a]", expired)
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

	if expired := mgr.ExpireIdle(); len(expired) != 1 {
		t.Fatalf("ExpireIdle() = %v, want one expired lease", expired)
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
