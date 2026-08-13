package ownership

import (
	"context"
	"errors"
	"os"
	"syscall"
	"time"
)

// Signaler is deliberately narrower than os.Process signaling: it can only
// signal a process group after the reconciler has verified its identity.
type Signaler interface {
	Supported() bool
	GroupSignal(pgid int, signal os.Signal) error
}

// Waiter waits for a verification predicate without spinning.
type Waiter interface {
	Wait(context.Context, time.Duration, func() (GroupStatus, error)) (GroupStatus, error)
}

type defaultWaiter struct{ interval time.Duration }

func (w defaultWaiter) Wait(ctx context.Context, timeout time.Duration, verify func() (GroupStatus, error)) (GroupStatus, error) {
	if timeout <= 0 {
		return verify()
	}
	interval := w.interval
	if interval <= 0 {
		interval = 25 * time.Millisecond
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		status, err := verify()
		if err != nil || status == GroupDead || status == GroupConflict {
			return status, err
		}
		select {
		case <-ctx.Done():
			return GroupUnknown, ctx.Err()
		case <-deadline.C:
			return status, nil
		case <-ticker.C:
		}
	}
}

// Result is a scrub-safe reconciliation result. It contains no lease token,
// process environment, or port information.
type Result struct {
	ServerName string
	Status     string
	Reason     string
}

// Reconciler reclaims only process groups whose complete identity is proven.
type Reconciler struct {
	Store       *Store
	ProcReader  ProcReader
	Signaler    Signaler
	Waiter      Waiter
	TermTimeout time.Duration
	KillTimeout time.Duration
}

func (r *Reconciler) Reconcile(ctx context.Context, expected map[string]ServerIdentity) []Result {
	if r.Store == nil {
		return nil
	}
	records, listErr := r.Store.List()
	if listErr != nil {
		return []Result{{ServerName: "<store>", Status: "error"}}
	}
	results := make([]Result, 0, len(records))
	for _, record := range records {
		result := Result{ServerName: record.ServerName}
		if record.Err != nil {
			result.Status = "error"
			results = append(results, result)
			continue
		}
		lease := record.Lease
		identity, ok := expected[record.ServerName]
		if !ok {
			result.Status = "conflict"
			results = append(results, result)
			continue
		}
		if r.ProcReader == nil {
			result.Status = "conflict"
			results = append(results, result)
			continue
		}
		status, verifyErr := VerifyGroup(r.ProcReader, lease, identity)
		if verifyErr != nil {
			result.Status, result.Reason = "error", "verification_failed"
			results = append(results, result)
			continue
		}
		if status == GroupConflict || status == GroupUnknown {
			result.Status = "conflict"
			results = append(results, result)
			continue
		}
		if status == GroupDead {
			if r.contextOK(ctx) && r.releaseIfCurrent(record.ServerName, lease) == nil {
				result.Status = "removed_dead"
			} else {
				result.Status = "conflict"
			}
			results = append(results, result)
			continue
		}
		if r.Signaler == nil || !r.Signaler.Supported() {
			result.Status = "unsupported"
			results = append(results, result)
			continue
		}
		if !r.contextOK(ctx) {
			result.Status = "error"
			results = append(results, result)
			continue
		}
		status, verifyErr = VerifyGroup(r.ProcReader, lease, identity)
		if verifyErr != nil {
			result.Status, result.Reason = "error", "verification_failed"
			results = append(results, result)
			continue
		}
		if status != GroupOwned {
			result.Status = scrubStatus(status)
			results = append(results, result)
			continue
		}
		if !r.contextOK(ctx) {
			result.Status = "error"
			results = append(results, result)
			continue
		}
		if err := r.Signaler.GroupSignal(lease.LeaderPGID, syscall.SIGTERM); err != nil {
			result.Status = "error"
			results = append(results, result)
			continue
		}
		waiter := r.Waiter
		if waiter == nil {
			waiter = defaultWaiter{}
		}
		status, err := waiter.Wait(ctx, r.TermTimeout, func() (GroupStatus, error) {
			if !r.contextOK(ctx) {
				return GroupUnknown, ctx.Err()
			}
			return VerifyGroup(r.ProcReader, lease, identity)
		})
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				result.Status, result.Reason = "error", "cancelled"
			} else {
				result.Status, result.Reason = "error", "verification_failed"
			}
			results = append(results, result)
			continue
		}
		if status == GroupConflict || status == GroupUnknown {
			result.Status, result.Reason = "conflict", "identity_changed"
			results = append(results, result)
			continue
		}
		if status == GroupDead {
			if r.contextOK(ctx) && r.releaseIfCurrent(record.ServerName, lease) == nil {
				result.Status = "reclaimed"
			} else {
				result.Status = "conflict"
			}
			results = append(results, result)
			continue
		}
		if !r.contextOK(ctx) {
			result.Status = "error"
			results = append(results, result)
			continue
		}
		status, verifyErr = VerifyGroup(r.ProcReader, lease, identity)
		if verifyErr != nil {
			result.Status, result.Reason = "error", "verification_failed"
			results = append(results, result)
			continue
		}
		if status != GroupOwned {
			result.Status = scrubStatus(status)
			results = append(results, result)
			continue
		}
		if !r.contextOK(ctx) {
			result.Status = "error"
			results = append(results, result)
			continue
		}
		if err := r.Signaler.GroupSignal(lease.LeaderPGID, syscall.SIGKILL); err != nil {
			result.Status = "error"
			results = append(results, result)
			continue
		}
		status, err = waiter.Wait(ctx, r.KillTimeout, func() (GroupStatus, error) {
			if !r.contextOK(ctx) {
				return GroupUnknown, ctx.Err()
			}
			return VerifyGroup(r.ProcReader, lease, identity)
		})
		if err == nil && status == GroupDead && r.contextOK(ctx) && r.releaseIfCurrent(record.ServerName, lease) == nil {
			result.Status = "reclaimed"
		} else if status == GroupConflict {
			result.Status = "conflict"
		} else {
			result.Status = "error"
		}
		results = append(results, result)
	}
	return results
}

func (r *Reconciler) ReconcileOne(ctx context.Context, name string, identity ServerIdentity) (Result, bool) {
	if r.Store == nil {
		return Result{ServerName: name, Status: "error"}, false
	}
	records, err := r.Store.List()
	if err != nil {
		return Result{ServerName: name, Status: "error", Reason: "store_unavailable"}, false
	}
	for _, record := range records {
		if record.ServerName == name {
			results := r.Reconcile(ctx, map[string]ServerIdentity{name: identity})
			for _, result := range results {
				if result.ServerName == name {
					return result, true
				}
			}
		}
	}
	return Result{ServerName: name}, false
}

func scrubStatus(status GroupStatus) string {
	if status == GroupConflict {
		return "conflict"
	}
	return "error"
}

func (r *Reconciler) contextOK(ctx context.Context) bool {
	return ctx.Err() == nil
}

func (r *Reconciler) releaseIfCurrent(name string, lease Lease) error {
	current, err := r.Store.Read(name)
	if err != nil {
		return err
	}
	if current.Generation != lease.Generation || current.DaemonID != lease.DaemonID || current.OwnerTokenHash != lease.OwnerTokenHash || current.ConfigHash != lease.ConfigHash || current.LeaderPID != lease.LeaderPID || current.LeaderPGID != lease.LeaderPGID || current.LeaderStart != lease.LeaderStart || current.BootID != lease.BootID || current.Executable != lease.Executable {
		return errors.New("lease changed")
	}
	return r.Store.ReleaseGeneration(name, lease.Generation)
}
