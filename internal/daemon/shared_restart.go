package daemon

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/admin"
	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/config"
	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/mcp"
	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/server"
)

const defaultSharedRestartDrainTimeout = 30 * time.Second

// precommitContext ends with its embedded context and also reports the
// daemon's shutdown as soon as it begins. The restart's commit point checks
// it under the registry lock, where a shutdown still propagating would let
// the stop commit after shutdown had dismissed the drain.
type precommitContext struct {
	context.Context
	shutdown context.Context
}

func (c precommitContext) Err() error {
	if err := c.shutdown.Err(); err != nil {
		return err
	}
	return c.Context.Err()
}

func (d *Daemon) sharedRestartDrainDuration() time.Duration {
	if d.sharedRestartDrainTimeout >= time.Millisecond {
		return d.sharedRestartDrainTimeout
	}
	return defaultSharedRestartDrainTimeout
}

// restartServer restarts one server under its lifecycle reservation, so no
// other stop, start or restart of that name interleaves with it.
//
// A shared-stdio proxy holds its running calls in a restart gate: the restart
// closes admission, waits for the calls, and cancels what remains at the
// deadline. That cancellation is irreversible, so it is bound to the stop's
// commit point. Until the commit, the request's end or the daemon's shutdown
// reopens the gate and leaves the server untouched. After the commit only
// shutdown applies, and the gate is never reopened.
//
// Every other server restarts through the registry without waiting for
// lifecycle event delivery.
func (d *Daemon) restartServer(ctx context.Context, name string) (*admin.RestartDrain, error) {
	reservation, err := d.registry.ReserveLifecycle(name)
	if err != nil {
		return nil, err
	}
	defer reservation.Release()

	cfg, _, err := reservation.Snapshot()
	if err != nil {
		return nil, err
	}
	if cfg.InferTransport() != config.TransportStdio || cfg.Stateful || cfg.SlotGroup != "" {
		_, err := reservation.Restart(context.Background(), context.Background(), false, nil)
		return nil, err
	}

	drain, cancel := context.WithCancel(ctx)
	defer cancel()
	stopLink := context.AfterFunc(d.restartCtx, cancel)
	defer stopLink()
	precommit := precommitContext{Context: drain, shutdown: d.restartCtx}

	if err := d.registry.WaitLifecycle(precommit, name); err != nil {
		return nil, err
	}
	_, state, err := reservation.Snapshot()
	if err != nil {
		return nil, err
	}
	value, hasGate := d.sharedRestartGates.Load(name)
	if !hasGate && state != server.StateStopped || hasGate && state != server.StateRunning {
		return nil, fmt.Errorf("shared proxy setup unavailable for %s in state %s", name, state)
	}

	timeout := d.sharedRestartDrainDuration()
	receipt := &admin.RestartDrain{
		Outcome:           "drained",
		TimeoutMS:         timeout.Milliseconds(),
		RemainingRequests: []uint64{},
	}
	var gate *mcp.SharedRestartGate
	if hasGate {
		gate = value.(*mcp.SharedRestartGate)
		if err := gate.Close(); err != nil {
			return nil, err
		}
		refs, err := gate.WaitDrained(precommit, timeout)
		if cause := precommit.Err(); cause != nil {
			gate.Abort()
			return nil, cause
		}
		if err != nil && !errors.Is(err, mcp.ErrSharedRestartDrainTimeout) {
			gate.Abort()
			return nil, err
		}
		if errors.Is(err, mcp.ErrSharedRestartDrainTimeout) {
			receipt.Outcome = "timed_out"
			for _, ref := range refs {
				receipt.RemainingRequests = append(receipt.RemainingRequests, uint64(ref))
			}
		}
	}

	committed, err := reservation.Restart(precommit, d.restartCtx, true, func() {
		if gate != nil && receipt.Outcome == "timed_out" {
			gate.CancelPending()
		}
	})
	if gate != nil && !committed {
		gate.Abort()
	}
	if err == nil {
		if fresh, ok := d.sharedRestartGates.Load(name); !ok || fresh == value {
			err = fmt.Errorf("shared proxy setup unavailable for %s", name)
		}
	}
	return receipt, err
}
