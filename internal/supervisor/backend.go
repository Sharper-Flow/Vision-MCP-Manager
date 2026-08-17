package supervisor

import (
	"context"
	"errors"
	"sync"
)

var ErrBackendUnavailable = errors.New("managed HTTP backend unavailable")
var ErrDrainContextHasDeadline = errors.New("drain context must not carry a deadline")

type BackendState string

const (
	BackendStarting   BackendState = "starting"
	BackendProbing    BackendState = "probing"
	BackendReady      BackendState = "ready"
	BackendDraining   BackendState = "draining"
	BackendRecycling  BackendState = "recycling"
	BackendRestarting BackendState = "restarting"
)

// BackendCoordinator serializes backend lifecycle changes with application
// requests. Cleanup-triggered recycle drains existing requests without
// cancelling them and rejects new dispatch while draining.
type BackendCoordinator struct {
	mu        sync.Mutex
	recycleMu sync.Mutex
	state     BackendState
	inFlight  int
	zeroCh    chan struct{}
}

func NewBackendCoordinator() *BackendCoordinator {
	zero := make(chan struct{})
	close(zero)
	return &BackendCoordinator{state: BackendStarting, zeroCh: zero}
}

func (c *BackendCoordinator) State() BackendState {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state
}

func (c *BackendCoordinator) Ready() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state == BackendReady
}

func (c *BackendCoordinator) MarkProbing() {
	c.mu.Lock()
	c.state = BackendProbing
	c.mu.Unlock()
}

func (c *BackendCoordinator) MarkReady() {
	c.mu.Lock()
	c.state = BackendReady
	c.mu.Unlock()
}

func (c *BackendCoordinator) MarkProcessLost() {
	c.mu.Lock()
	c.state = BackendRestarting
	c.mu.Unlock()
}

func (c *BackendCoordinator) BeginRequest() (func(), error) {
	c.mu.Lock()
	if c.state != BackendReady {
		c.mu.Unlock()
		return nil, ErrBackendUnavailable
	}
	if c.inFlight == 0 {
		c.zeroCh = make(chan struct{})
	}
	c.inFlight++
	c.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			c.mu.Lock()
			defer c.mu.Unlock()
			if c.inFlight == 0 {
				return
			}
			c.inFlight--
			if c.inFlight == 0 {
				close(c.zeroCh)
			}
		})
	}, nil
}

func (c *BackendCoordinator) InFlight() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.inFlight
}

// DrainAndRecycle blocks new dispatch, waits for every admitted application
// request to finish, then invokes recycle exactly once. Successful recycle
// transitions to probing; callers must complete readiness before MarkReady.
func (c *BackendCoordinator) DrainAndRecycle(ctx context.Context, recycle func() error) error {
	if _, hasDeadline := ctx.Deadline(); hasDeadline {
		return ErrDrainContextHasDeadline
	}

	c.recycleMu.Lock()
	defer c.recycleMu.Unlock()

	c.mu.Lock()
	if c.state != BackendReady {
		c.mu.Unlock()
		return ErrBackendUnavailable
	}
	c.state = BackendDraining
	wait := c.zeroCh
	c.mu.Unlock()

	select {
	case <-ctx.Done():
		c.mu.Lock()
		c.state = BackendRestarting
		c.mu.Unlock()
		return ctx.Err()
	case <-wait:
	}

	c.mu.Lock()
	c.state = BackendRecycling
	c.mu.Unlock()

	if err := recycle(); err != nil {
		c.mu.Lock()
		c.state = BackendRestarting
		c.mu.Unlock()
		return err
	}

	c.mu.Lock()
	c.state = BackendProbing
	c.mu.Unlock()
	return nil
}
