package ownership

// These tests specify the reconciliation boundary.  They intentionally use a
// fake ProcReader and Signaler: a reconciler must prove ownership immediately
// before TERM and again before KILL, and must never turn an unverifiable group
// into a kill target.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
)

type reconcileGroup struct {
	status      GroupStatus
	verifyCount int
	onVerify    map[int]GroupStatus
	onTerm      func()
	onKill      func()
	callbackErr error
	bootID      string
	leaderPID   int
	leaderPGID  int
	leaderStart uint64
	token       string
	executable  string
}

func (g *reconcileGroup) ReadBootID() (string, error) {
	g.verifyCount++
	if next, ok := g.onVerify[g.verifyCount]; ok {
		g.status = next
	}
	if g.status == GroupUnknown {
		return "", errors.New("unknown boot identity")
	}
	return g.bootID, nil
}

func (g *reconcileGroup) ReadStat(pid int) (ProcStat, error) {
	if g.status == GroupDead {
		return ProcStat{}, errors.New("process exited")
	}
	if g.status == GroupUnknown {
		return ProcStat{}, errors.New("stat unreadable")
	}
	stat := ProcStat{PID: pid, PGRP: g.leaderPGID, StartTime: g.leaderStart}
	if g.status == GroupConflict {
		stat.PGRP++
	}
	return stat, nil
}

func (g *reconcileGroup) ReadEnviron(pid int) ([]byte, error) {
	if g.status != GroupOwned {
		return nil, errors.New("environment unavailable")
	}
	return []byte(OwnerTokenEnvKey + "=" + g.token + "\x00"), nil
}

func (g *reconcileGroup) ReadExe(pid int) (string, error) {
	if g.status == GroupUnknown {
		return "", errors.New("executable unreadable")
	}
	return g.executable, nil
}

func (g *reconcileGroup) ListMembers(pgid int) ([]int, error) {
	if g.status == GroupUnknown {
		return nil, errors.New("members unreadable")
	}
	if g.status == GroupDead {
		return nil, nil
	}
	return []int{g.leaderPID}, nil
}

type recordedGroupSignal struct {
	pgid int
	sig  os.Signal
}

type fakeSignaler struct {
	signals         []recordedGroupSignal
	groupByPGID     map[int]*reconcileGroup
	termErrByPGID   map[int]error
	killErrByPGID   map[int]error
	supported       bool
	groupSignalCall int
}

func (s *fakeSignaler) Supported() bool { return s.supported }

func (s *fakeSignaler) GroupSignal(pgid int, sig os.Signal) error {
	s.groupSignalCall++
	s.signals = append(s.signals, recordedGroupSignal{pgid: pgid, sig: sig})
	group := s.groupByPGID[pgid]
	if group == nil {
		return errors.New("unknown process group")
	}
	switch sig {
	case syscall.SIGTERM:
		if err := s.termErrByPGID[pgid]; err != nil {
			return err
		}
		if group.onTerm != nil {
			group.onTerm()
		}
	case syscall.SIGKILL:
		if err := s.killErrByPGID[pgid]; err != nil {
			return err
		}
		if group.onKill != nil {
			group.onKill()
		}
	}
	return nil
}

// Wait is deliberately synchronous. It gives tests a deterministic hook at
// the exact boundary between TERM's wait and the KILL decision.
type fakeReconcileWaiter struct {
	onWait func()
}

func (w *fakeReconcileWaiter) Wait(ctx context.Context, timeout time.Duration, verify func() (GroupStatus, error)) (GroupStatus, error) {
	if w.onWait != nil {
		w.onWait()
	}
	return verify()
}

func testIdentity(name string) ServerIdentity {
	return ServerIdentity{
		Name: name, Command: "/vision/fixture", Transport: "managed-http",
	}
}

func testLease(name, token string, generation uint64) Lease {
	identity := testIdentity(name)
	return Lease{
		Version:        LeaseSchemaVersion,
		Generation:     generation,
		ServerName:     name,
		DaemonID:       "test-daemon",
		OwnerTokenHash: TokenHash(token),
		ConfigHash:     ConfigHash(identity),
		LeaderPID:      int(generation) + 100,
		LeaderPGID:     int(generation) + 100,
		LeaderStart:    500,
		BootID:         "test-boot",
		Executable:     identity.Command,
		CreatedAt:      time.Unix(1, 0),
	}
}

func newReconcileFixture(t *testing.T, names ...string) (*Store, map[string]ServerIdentity, map[string]*reconcileGroup, *fakeSignaler, *fakeReconcileWaiter) {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	expected := make(map[string]ServerIdentity, len(names))
	groups := make(map[string]*reconcileGroup, len(names))
	for i, name := range names {
		token := "token-" + name
		lease := testLease(name, token, uint64(i+1))
		if err := store.Record(name, lease); err != nil {
			t.Fatal(err)
		}
		groups[name] = &reconcileGroup{
			status:      GroupOwned,
			onVerify:    make(map[int]GroupStatus),
			bootID:      lease.BootID,
			leaderPID:   lease.LeaderPID,
			leaderPGID:  lease.LeaderPGID,
			leaderStart: lease.LeaderStart,
			token:       token,
			executable:  lease.Executable,
		}
		expected[name] = testIdentity(name)
	}
	groupByPGID := make(map[int]*reconcileGroup, len(groups))
	for _, group := range groups {
		groupByPGID[group.leaderPGID] = group
	}
	signaler := &fakeSignaler{
		groupByPGID:   groupByPGID,
		termErrByPGID: make(map[int]error),
		killErrByPGID: make(map[int]error),
		supported:     true,
	}
	waiter := &fakeReconcileWaiter{}
	return store, expected, groups, signaler, waiter
}

func newReconcilerForTest(store *Store, reader ProcReader, signaler *fakeSignaler, waiter *fakeReconcileWaiter) *Reconciler {
	return &Reconciler{
		Store:       store,
		ProcReader:  reader,
		Signaler:    signaler,
		Waiter:      waiter,
		TermTimeout: time.Second,
		KillTimeout: time.Second,
	}
}

func signalerForGroups(groups map[string]*reconcileGroup) *fakeSignaler {
	byPGID := make(map[int]*reconcileGroup, len(groups))
	for _, group := range groups {
		byPGID[group.leaderPGID] = group
	}
	return &fakeSignaler{
		groupByPGID:   byPGID,
		termErrByPGID: make(map[int]error),
		killErrByPGID: make(map[int]error),
		supported:     true,
	}
}

func statusOf(t *testing.T, result Result) string {
	t.Helper()
	return fmt.Sprint(result.Status)
}

func requireResult(t *testing.T, results []Result, name, wantStatus string) Result {
	t.Helper()
	for _, result := range results {
		if result.ServerName == name {
			if got := statusOf(t, result); got != wantStatus {
				t.Fatalf("result for %q status = %q, want %q: %#v", name, got, wantStatus, result)
			}
			return result
		}
	}
	t.Fatalf("missing result for %q in %#v", name, results)
	return Result{}
}

func TestReconcileReverifiesImmediatelyBeforeTERM(t *testing.T) {
	store, expected, groups, signaler, waiter := newReconcileFixture(t, "browser")
	group := groups["browser"]
	group.onVerify[2] = GroupConflict
	reconciler := newReconcilerForTest(store, group, signaler, waiter)

	results := reconciler.Reconcile(context.Background(), expected)
	requireResult(t, results, "browser", "conflict")
	if len(signaler.signals) != 0 {
		t.Fatalf("signals after identity changed before TERM = %#v, want none", signaler.signals)
	}
	if _, err := store.Read("browser"); err != nil {
		t.Fatalf("conflicting lease was removed: %v", err)
	}
}

func TestReconcileTERMThenDeadRemovesLeaseAndReportsReclaimed(t *testing.T) {
	store, expected, groups, signaler, waiter := newReconcileFixture(t, "browser")
	group := groups["browser"]
	group.onTerm = func() { group.status = GroupDead }
	reconciler := newReconcilerForTest(store, group, signaler, waiter)

	results := reconciler.Reconcile(context.Background(), expected)
	requireResult(t, results, "browser", "reclaimed")
	if len(signaler.signals) != 1 || signaler.signals[0].sig != syscall.SIGTERM {
		t.Fatalf("signals = %#v, want one TERM", signaler.signals)
	}
	if _, err := store.Read("browser"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("lease after dead-group reclaim = %v, want not-exist", err)
	}
}

func TestReconcileTERMResistantReverifiesBeforeKILL(t *testing.T) {
	store, expected, groups, signaler, waiter := newReconcileFixture(t, "browser")
	group := groups["browser"]
	group.onTerm = func() { group.status = GroupOwned }
	group.onKill = func() { group.status = GroupDead }
	reconciler := newReconcilerForTest(store, group, signaler, waiter)

	results := reconciler.Reconcile(context.Background(), expected)
	requireResult(t, results, "browser", "reclaimed")
	if len(signaler.signals) != 2 || signaler.signals[0].sig != syscall.SIGTERM || signaler.signals[1].sig != syscall.SIGKILL {
		t.Fatalf("signals = %#v, want TERM then KILL", signaler.signals)
	}
}

func TestReconcileIdentityChangeBetweenTERMWaitAndKILLRetainsLease(t *testing.T) {
	store, expected, groups, signaler, waiter := newReconcileFixture(t, "browser")
	group := groups["browser"]
	group.onTerm = func() { group.status = GroupOwned }
	waiter.onWait = func() { group.status = GroupConflict }
	reconciler := newReconcilerForTest(store, group, signaler, waiter)

	results := reconciler.Reconcile(context.Background(), expected)
	requireResult(t, results, "browser", "conflict")
	if len(signaler.signals) != 1 || signaler.signals[0].sig != syscall.SIGTERM {
		t.Fatalf("signals = %#v, want TERM only", signaler.signals)
	}
	if _, err := store.Read("browser"); err != nil {
		t.Fatalf("conflicting lease was removed: %v", err)
	}
}

func TestReconcileDeadGroupRemovesLeaseWithoutSignal(t *testing.T) {
	store, expected, groups, signaler, waiter := newReconcileFixture(t, "browser")
	group := groups["browser"]
	group.status = GroupDead
	reconciler := newReconcilerForTest(store, group, signaler, waiter)

	results := reconciler.Reconcile(context.Background(), expected)
	requireResult(t, results, "browser", "removed_dead")
	if len(signaler.signals) != 0 {
		t.Fatalf("signals for dead group = %#v, want none", signaler.signals)
	}
	if _, err := store.Read("browser"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dead lease remains: %v", err)
	}
}

func TestReconcileMissingConfigMismatchMixedTokenAndUnknownReadRetainLease(t *testing.T) {
	tests := []struct {
		name       string
		expected   map[string]ServerIdentity
		groupState GroupStatus
		mutate     func(*reconcileGroup)
		want       string
	}{
		{name: "missing expected config", expected: map[string]ServerIdentity{}, want: "conflict"},
		{name: "config mismatch", expected: map[string]ServerIdentity{"browser": {Name: "browser", Command: "/other", Transport: "managed-http"}}, want: "conflict"},
		{name: "mixed token", groupState: GroupConflict, want: "conflict"},
		{name: "unknown read", groupState: GroupUnknown, want: "conflict"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store, expected, groups, signaler, waiter := newReconcileFixture(t, "browser")
			group := groups["browser"]
			if tc.groupState != "" {
				group.status = tc.groupState
			}
			if tc.mutate != nil {
				tc.mutate(group)
			}
			if tc.expected != nil {
				expected = tc.expected
			}
			reconciler := newReconcilerForTest(store, group, signaler, waiter)

			results := reconciler.Reconcile(context.Background(), expected)
			requireResult(t, results, "browser", tc.want)
			if len(signaler.signals) != 0 {
				t.Fatalf("signals = %#v, want none", signaler.signals)
			}
			if _, err := store.Read("browser"); err != nil {
				t.Fatalf("lease was removed: %v", err)
			}
		})
	}
}

func TestReconcileIndependentLeaseConflictsHaveDeterministicResults(t *testing.T) {
	store, expected, groups, _, waiter := newReconcileFixture(t, "zombie", "browser")
	groups["zombie"].status = GroupConflict
	groups["browser"].onTerm = func() { groups["browser"].status = GroupDead }
	// The production signaler must receive the PGID for the lease being
	// processed, not a shared process/port identifier.
	signaler := signalerForGroups(groups)
	reconciler := newReconcilerForTest(store, &perNameReader{groups: groups}, signaler, waiter)

	results := reconciler.Reconcile(context.Background(), expected)
	if got := []string{results[0].ServerName, results[1].ServerName}; !reflect.DeepEqual(got, []string{"browser", "zombie"}) {
		t.Fatalf("result order = %#v, want [browser zombie]", got)
	}
	requireResult(t, results, "zombie", "conflict")
	requireResult(t, results, "browser", "reclaimed")
	if len(signaler.signals) != 1 || signaler.signals[0].pgid != groups["browser"].leaderPGID {
		t.Fatalf("signals = %#v, want independent browser PGID only", signaler.signals)
	}
}

func TestReconcileDiscoversMalformedOrUnsafeLeaseWithoutBlockingValidLease(t *testing.T) {
	tests := []struct {
		name  string
		write func(t *testing.T, path string)
	}{
		{
			name: "malformed lease",
			write: func(t *testing.T, path string) {
				if err := os.WriteFile(path, []byte("not-json"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "unsafe symlink lease",
			write: func(t *testing.T, path string) {
				target := t.TempDir() + "/outside"
				if err := os.WriteFile(target, []byte("{}"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store, expected, groups, _, waiter := newReconcileFixture(t, "valid")
			brokenPath, err := store.Path("broken")
			if err != nil {
				t.Fatal(err)
			}
			tc.write(t, brokenPath)
			groups["valid"].onTerm = func() { groups["valid"].status = GroupDead }
			signaler := signalerForGroups(groups)
			reconciler := newReconcilerForTest(store, &perNameReader{groups: groups}, signaler, waiter)

			results := reconciler.Reconcile(context.Background(), expected)
			requireResult(t, results, "broken", "error")
			requireResult(t, results, "valid", "reclaimed")
			if len(signaler.signals) != 1 || signaler.signals[0].pgid != groups["valid"].leaderPGID {
				t.Fatalf("signals = %#v, want valid lease PGID only", signaler.signals)
			}
			if _, err := store.Read("broken"); err == nil {
				t.Fatal("malformed or unsafe lease unexpectedly became readable")
			}
		})
	}
}

type perNameReader struct{ groups map[string]*reconcileGroup }

// The reconciler has no port input; this reader maps the lease leader PID to
// its fake group solely so the test can model two independent process groups.
func (r *perNameReader) group(pid int) *reconcileGroup {
	for _, group := range r.groups {
		if group.leaderPID == pid {
			return group
		}
	}
	return &reconcileGroup{status: GroupUnknown, onVerify: map[int]GroupStatus{}}
}
func (r *perNameReader) ReadBootID() (string, error)         { return "test-boot", nil }
func (r *perNameReader) ReadStat(pid int) (ProcStat, error)  { return r.group(pid).ReadStat(pid) }
func (r *perNameReader) ReadEnviron(pid int) ([]byte, error) { return r.group(pid).ReadEnviron(pid) }
func (r *perNameReader) ReadExe(pid int) (string, error)     { return r.group(pid).ReadExe(pid) }
func (r *perNameReader) ListMembers(pgid int) ([]int, error) {
	for _, group := range r.groups {
		if group.leaderPGID == pgid {
			return group.ListMembers(pgid)
		}
	}
	return nil, errors.New("unknown process group")
}

func TestReconcileUnsupportedSignalerDoesNotSignalOrRemove(t *testing.T) {
	store, expected, groups, _, waiter := newReconcileFixture(t, "browser")
	group := groups["browser"]
	signaler := signalerForGroups(groups)
	signaler.supported = false
	reconciler := newReconcilerForTest(store, group, signaler, waiter)

	results := reconciler.Reconcile(context.Background(), expected)
	requireResult(t, results, "browser", "unsupported")
	if signaler.groupSignalCall != 0 {
		t.Fatalf("GroupSignal calls from unsupported signaler = %d, want zero", signaler.groupSignalCall)
	}
	if _, err := store.Read("browser"); err != nil {
		t.Fatalf("unsupported lease was removed: %v", err)
	}
}

func TestReconcileTERMErrorRetainsLeaseAndDoesNotKILL(t *testing.T) {
	store, expected, groups, _, waiter := newReconcileFixture(t, "browser")
	group := groups["browser"]
	signaler := signalerForGroups(groups)
	signaler.termErrByPGID[group.leaderPGID] = errors.New("term failed")
	reconciler := newReconcilerForTest(store, group, signaler, waiter)

	results := reconciler.Reconcile(context.Background(), expected)
	requireResult(t, results, "browser", "error")
	if len(signaler.signals) != 1 || signaler.signals[0].sig != syscall.SIGTERM {
		t.Fatalf("signals = %#v, want TERM only", signaler.signals)
	}
	if _, err := store.Read("browser"); err != nil {
		t.Fatalf("lease after TERM error = %v", err)
	}
}

func TestReconcileKILLErrorRetainsLeaseAndReportsError(t *testing.T) {
	store, expected, groups, _, waiter := newReconcileFixture(t, "browser")
	group := groups["browser"]
	group.onTerm = func() { group.status = GroupOwned }
	signaler := signalerForGroups(groups)
	signaler.killErrByPGID[group.leaderPGID] = errors.New("kill failed")
	reconciler := newReconcilerForTest(store, group, signaler, waiter)

	results := reconciler.Reconcile(context.Background(), expected)
	requireResult(t, results, "browser", "error")
	if len(signaler.signals) != 2 || signaler.signals[1].sig != syscall.SIGKILL {
		t.Fatalf("signals = %#v, want TERM then KILL", signaler.signals)
	}
	if _, err := store.Read("browser"); err != nil {
		t.Fatalf("lease after KILL error = %v", err)
	}
}

func TestReconcileCancelledBeforeActionRetainsLeaseWithoutSignal(t *testing.T) {
	store, expected, groups, _, waiter := newReconcileFixture(t, "browser")
	signaler := signalerForGroups(groups)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	reconciler := newReconcilerForTest(store, groups["browser"], signaler, waiter)

	results := reconciler.Reconcile(ctx, expected)
	requireResult(t, results, "browser", "error")
	if signaler.groupSignalCall != 0 {
		t.Fatalf("GroupSignal calls for cancelled reconcile = %d, want zero", signaler.groupSignalCall)
	}
	if _, err := store.Read("browser"); err != nil {
		t.Fatalf("cancelled reconcile removed lease: %v", err)
	}
}

func TestReconcileGenerationChangeCannotRemoveNewLease(t *testing.T) {
	store, expected, groups, signaler, waiter := newReconcileFixture(t, "browser")
	group := groups["browser"]
	oldLease, err := store.Read("browser")
	if err != nil {
		t.Fatal(err)
	}
	group.onTerm = func() {
		newLease := oldLease
		newLease.Generation++
		newLease.OwnerTokenHash = TokenHash("new-generation-token")
		if err := store.Record("browser", newLease); err != nil {
			group.callbackErr = err
			return
		}
		group.status = GroupDead
	}
	reconciler := newReconcilerForTest(store, group, signaler, waiter)

	results := reconciler.Reconcile(context.Background(), expected)
	if group.callbackErr != nil {
		t.Fatalf("record new generation: %v", group.callbackErr)
	}
	requireResult(t, results, "browser", "conflict")
	current, err := store.Read("browser")
	if err != nil {
		t.Fatal(err)
	}
	if current.Generation != oldLease.Generation+1 {
		t.Fatalf("current generation = %d, want %d", current.Generation, oldLease.Generation+1)
	}
}

func TestReconcileHasNoPortInputAndIgnoresUnleasedProcess(t *testing.T) {
	store, expected, groups, signaler, waiter := newReconcileFixture(t)
	group := &reconcileGroup{
		status:      GroupOwned,
		bootID:      "test-boot",
		leaderPID:   999,
		leaderPGID:  999,
		leaderStart: 500,
		token:       "unleased-token",
		executable:  "/vision/fixture",
		onVerify:    map[int]GroupStatus{},
	}
	groups["unleased"] = group
	// Compile-time API assertion: a port cannot be supplied to Reconcile.
	// Written as a conversion rather than a typed declaration because the type
	// is the whole point of the line, and staticcheck's QF1011 would otherwise
	// insist on inferring it away — which would silently delete the assertion.
	// A conversion checks the signature just as strictly and has nothing to infer.
	_ = (func(context.Context, map[string]ServerIdentity) []Result)((&Reconciler{}).Reconcile)
	reconciler := newReconcilerForTest(store, group, signaler, waiter)
	results := reconciler.Reconcile(context.Background(), expected)
	if len(results) != 0 {
		t.Fatalf("results for unleased process = %#v, want none", results)
	}
	if len(signaler.signals) != 0 {
		t.Fatalf("signals for unleased process = %#v, want none", signaler.signals)
	}
}

func TestReconcileResultDoesNotExposeRawOwnerToken(t *testing.T) {
	store, expected, groups, signaler, waiter := newReconcileFixture(t, "browser")
	groups["browser"].status = GroupConflict
	reconciler := newReconcilerForTest(store, groups["browser"], signaler, waiter)
	results := reconciler.Reconcile(context.Background(), expected)
	for _, result := range results {
		encoded := fmt.Sprintf("%#v", result)
		if strings.Contains(encoded, "token-browser") {
			t.Fatalf("result exposes raw token: %s", encoded)
		}
	}
}
