package daemon

import (
	"context"
	"errors"
	"time"

	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/admin"
	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/mcp"
)

const defaultSharedRestartDrainTimeout = 30 * time.Second

func (d *Daemon) restartServer(ctx context.Context, name string) (*admin.RestartDrain, error) {
	if err := d.registry.WaitLifecycle(ctx, name); err != nil {
		return nil, err
	}
	value, ok := d.sharedRestartGates.Load(name)
	if !ok {
		return nil, d.registry.Restart(name)
	}
	gate := value.(*mcp.SharedRestartGate)
	if err := gate.Close(); err != nil {
		return nil, err
	}
	timeout := d.sharedRestartDrainTimeout
	if timeout < time.Millisecond {
		timeout = defaultSharedRestartDrainTimeout
	}
	refs, err := gate.WaitDrained(ctx, timeout)
	if ctx.Err() != nil {
		gate.Abort()
		return nil, ctx.Err()
	}
	if err != nil && !errors.Is(err, mcp.ErrSharedRestartDrainTimeout) {
		gate.Abort()
		return nil, err
	}
	receipt := &admin.RestartDrain{
		Outcome:           "drained",
		TimeoutMS:         timeout.Milliseconds(),
		RemainingRequests: make([]uint64, 0, len(refs)),
	}
	if errors.Is(err, mcp.ErrSharedRestartDrainTimeout) {
		receipt.Outcome = "timed_out"
		for _, ref := range refs {
			receipt.RemainingRequests = append(receipt.RemainingRequests, uint64(ref))
		}
		gate.CancelPending()
	}
	return receipt, d.registry.RestartTracked(d.ctx, name)
}
