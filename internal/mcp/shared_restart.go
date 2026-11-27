package mcp

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var (
	// ErrSharedRestartClosed refuses an application request that arrived after
	// a restart closed the gate.
	ErrSharedRestartClosed = errors.New("shared downstream restart in progress")
	// ErrSharedRestartInProgress refuses a second restart of the same gate.
	ErrSharedRestartInProgress = errors.New("shared downstream restart already in progress")
	// ErrSharedRestartDrainTimeout reports that a drain wait used up its duration.
	ErrSharedRestartDrainTimeout = errors.New("shared downstream restart drain timed out")
)

// SharedRestartRef names one admitted request inside a SharedRestartGate. It is
// an admission counter, so it is safe to log and to return to an operator: it
// derives from no session identifier, JSON-RPC ID, or payload.
type SharedRestartRef uint64

// SharedRestartGate holds the application requests of one shared-stdio proxy
// so a targeted restart can let them finish before the old listener closes.
//
// A restart coordinator calls it in this order:
//
//  1. Close refuses every later application request. Only the first Close of a
//     gate succeeds; another restart of the same proxy meanwhile gets
//     ErrSharedRestartInProgress.
//  2. WaitDrained blocks until every held request has written its response,
//     the coordinator's context ends, or the passed duration elapses.
//  3. CancelPending, only once the coordinator commits to the cutover at that
//     deadline, ends the SDK forwarding contexts of the requests still held.
//
// Neither a timeout nor a completed drain reopens admission. Abort reopens it,
// and the coordinator calls Abort only for a restart abandoned before the
// replacement downstream is admitted.
//
// Held means an application request: a POST whose envelope carries a client
// request other than ping. Ping, responses, notifications, GET/SSE and DELETE
// are neither held nor counted, because they create no downstream work or may
// support a request already in flight. A held request stays held until its
// HTTP handler returns, after the response is written or the caller is gone.
type SharedRestartGate struct {
	mu      sync.Mutex
	closed  bool
	nextRef SharedRestartRef
	held    map[SharedRestartRef]struct{}

	// idle is closed while held is empty. Admitting into an empty gate
	// replaces it with an open channel, so a waiter can select on it.
	idle chan struct{}

	// cutover ends when CancelPending commits. Abort replaces it, so a
	// reopened gate starts with no committed cutover.
	cutover       context.Context
	cutoverCancel context.CancelFunc
}

// NewSharedRestartGate returns an open gate that holds no request.
func NewSharedRestartGate() *SharedRestartGate {
	cutover, cancel := context.WithCancel(context.Background())
	idle := make(chan struct{})
	close(idle)
	return &SharedRestartGate{
		held:          make(map[SharedRestartRef]struct{}),
		idle:          idle,
		cutover:       cutover,
		cutoverCancel: cancel,
	}
}

// Close stops admitting application requests. The first call succeeds and
// makes the caller the only restart owner of the gate; every later call
// returns ErrSharedRestartInProgress without waiting for held requests. A
// request admitted before Close keeps running.
func (g *SharedRestartGate) Close() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return ErrSharedRestartInProgress
	}
	g.closed = true
	return nil
}

// WaitDrained waits until no request is held, the coordinator's context ends,
// or timeout elapses, whichever happens first. It returns no reference when
// every held request finished, the references still held with ctx.Err() when
// the context ends first, and the references still held with
// ErrSharedRestartDrainTimeout when the duration ends first. A timeout of zero
// or less reports the held requests without waiting. A request released while
// the wait ends counts as drained: an error always comes with the references
// still held.
//
// WaitDrained changes no admission state. Close first, or new requests may be
// admitted while it waits.
func (g *SharedRestartGate) WaitDrained(ctx context.Context, timeout time.Duration) ([]SharedRestartRef, error) {
	g.mu.Lock()
	idle := g.idle
	pending := len(g.held)
	g.mu.Unlock()
	if pending == 0 {
		return nil, nil
	}
	if timeout <= 0 {
		return g.undrained(ErrSharedRestartDrainTimeout)
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-idle:
		return nil, nil
	case <-ctx.Done():
		return g.undrained(ctx.Err())
	case <-timer.C:
		return g.undrained(ErrSharedRestartDrainTimeout)
	}
}

// CancelPending commits the cutover: every method the SDK handles on the proxy
// from now on, including the ones already forwarding, receives a cancelled
// forwarding context, so the SDK retires its outgoing downstream calls. The
// downstream is neither killed nor replayed. Call it only at the committed
// drain deadline, after Close; on an open gate it does nothing. It returns the
// references still held.
func (g *SharedRestartGate) CancelPending() []SharedRestartRef {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.closed {
		return nil
	}
	g.cutoverCancel()
	return g.pendingLocked()
}

// Abort reopens admission after a restart that was abandoned before the
// replacement downstream was admitted. Requests held before Close stay held.
// On an open gate it does nothing.
func (g *SharedRestartGate) Abort() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.closed {
		return
	}
	g.closed = false
	if g.cutover.Err() != nil {
		g.cutover, g.cutoverCancel = context.WithCancel(context.Background())
	}
}

// enter admits one application request and returns its reference and a
// release that is safe to call more than once.
func (g *SharedRestartGate) enter() (SharedRestartRef, func(), error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return 0, nil, ErrSharedRestartClosed
	}
	g.nextRef++
	ref := g.nextRef
	if len(g.held) == 0 {
		g.idle = make(chan struct{})
	}
	g.held[ref] = struct{}{}
	var once sync.Once
	release := func() {
		once.Do(func() {
			g.mu.Lock()
			defer g.mu.Unlock()
			delete(g.held, ref)
			if len(g.held) == 0 {
				close(g.idle)
			}
		})
	}
	return ref, release, nil
}

// admit takes restart admission for an application request. The returned
// release is a no-op when the request is not held, including when its body
// cannot be read or classified; the SDK then rejects the request as before.
func (g *SharedRestartGate) admit(r *http.Request) (func(), error) {
	if r.Method != http.MethodPost || r.Body == nil {
		return func() {}, nil
	}
	body, err := readAndRestoreRequestBody(r)
	if err != nil {
		return func() {}, nil
	}
	active, err := ClassifyApplicationActivity(body)
	if err != nil || !active {
		return func() {}, nil
	}
	_, release, err := g.enter()
	if err != nil {
		return nil, err
	}
	return release, nil
}

// bindCutover returns a child of ctx that ends when the gate's cutover is
// committed. A binding made after CancelPending is cancelled at once.
func (g *SharedRestartGate) bindCutover(ctx context.Context) (context.Context, context.CancelFunc) {
	g.mu.Lock()
	cutover := g.cutover
	g.mu.Unlock()
	bound, cancel := context.WithCancel(ctx)
	if cutover.Err() != nil {
		cancel()
		return bound, cancel
	}
	stop := context.AfterFunc(cutover, cancel)
	return bound, func() {
		stop()
		cancel()
	}
}

func (g *SharedRestartGate) pending() []SharedRestartRef {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.pendingLocked()
}

func (g *SharedRestartGate) pendingLocked() []SharedRestartRef {
	if len(g.held) == 0 {
		return nil
	}
	refs := make([]SharedRestartRef, 0, len(g.held))
	for ref := range g.held {
		refs = append(refs, ref)
	}
	slices.Sort(refs)
	return refs
}

// undrained returns the references still held with err, or reports the drain
// complete when none remain.
func (g *SharedRestartGate) undrained(err error) ([]SharedRestartRef, error) {
	refs := g.pending()
	if refs == nil {
		return nil, nil
	}
	return refs, err
}

// sharedRestartCutover is the receiving middleware of a shared proxy: every
// method the SDK dispatches forwards under a context that ends at cutover.
func sharedRestartCutover(gate *SharedRestartGate) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			ctx, release := gate.bindCutover(ctx)
			defer release()
			return next(ctx, method, req)
		}
	}
}
