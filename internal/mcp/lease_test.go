package mcp

import (
	"errors"
	"sync"
	"testing"
	"time"
)

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
