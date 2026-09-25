// Package ops decorates the session-lifetime operation.Manager.
//
// Translators run synchronously on the coordinator goroutine and must not
// block (harness/coordinator/loop.go:780-814, tool/tool.go:19-23), but the
// coordinator persists a call's ToolCallStatus (and so notifies store
// observers) BEFORE it hands the call's operations to the manager
// (loop.go:233-246, 952-961). So approval is gated at Add: a gated op is held
// in StatusReady (the coordinator only sees "still running"), the client is
// asked asynchronously, and the op is either forwarded to the real manager or
// answered with a synthesized terminal snapshot (StatusCanceled with a
// TerminalError reason, which bash.TranslateResult renders for the model).
//
// Two library hazards are handled here:
//   - LocalOperationManager silently drops a duplicate Add of a known ID
//     (operation/local_manager.go:133-137), and a coordinator generation that
//     dies inside slurpChannel discards updates it already dequeued
//     (coordinator/loop.go:436-440). The decorator remembers the latest
//     snapshot per ID and re-emits it when a new generation re-Adds a known ID.
//   - StopHard waits until every op is terminal (loop.go:261-270), so a
//     Cancel of a held op must answer with a terminal snapshot.
package ops

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"sync"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/operation"

	"github.com/gastownhall/acp-unreal/internal/fifo"
)

// Verdict is a policy decision for a ready operation.
type Verdict int

// Verdicts.
const (
	Allow Verdict = iota + 1
	Ask
	Deny
)

// Options configures a Manager.
type Options struct {
	// Policy decides a StatusReady op; reason is used for Deny.
	Policy func(operation.Operation) (verdict Verdict, reason string)
	// Ask starts an asynchronous approval; it must eventually call Decide
	// (or the op is resolved by Cancel). ctx ends when the op is decided.
	Ask func(ctx context.Context, op operation.Operation)
	// Tap sees every snapshot before it is delivered, in order.
	Tap func(operation.Operation)
	// OpTimeout bounds each forwarded op's wall clock (0 = unbounded).
	OpTimeout time.Duration
}

type heldOp struct {
	op     operation.Operation
	cancel context.CancelFunc
}

// Manager is an operation.Manager decorator. Its Updates channel never
// closes; Drained is closed once the inner manager's Updates closes.
type Manager struct {
	ctx     context.Context
	inner   operation.Manager
	opts    Options
	updates *fifo.Queue[operation.Operation]
	out     chan operation.Operation
	drained chan struct{}

	mu     sync.Mutex
	held   map[operation.ID]*heldOp
	latest map[operation.ID]operation.Operation
	timers map[operation.ID]*time.Timer
}

var _ operation.Manager = (*Manager)(nil)

// New wraps inner. ctx must be the inner manager's ctx: when it ends, held
// ops are abandoned and delivery stops.
func New(ctx context.Context, inner operation.Manager, opts Options) *Manager {
	if opts.Policy == nil {
		opts.Policy = func(operation.Operation) (Verdict, string) { return Allow, "" }
	}
	if opts.Tap == nil {
		opts.Tap = func(operation.Operation) {}
	}
	m := &Manager{
		ctx: ctx, inner: inner, opts: opts,
		updates: fifo.New[operation.Operation](), out: make(chan operation.Operation),
		drained: make(chan struct{}),
		held:    map[operation.ID]*heldOp{}, latest: map[operation.ID]operation.Operation{},
		timers: map[operation.ID]*time.Timer{},
	}
	go m.pump()
	go m.feed()
	return m
}

// Add implements operation.Manager.
func (m *Manager) Add(op operation.Operation) error {
	m.mu.Lock()
	if _, isHeld := m.held[op.ID]; isHeld {
		m.mu.Unlock()
		return nil // approval still pending; the ask stands
	}
	if latest, known := m.latest[op.ID]; known {
		m.mu.Unlock()
		m.emit(latest) // the inner manager would drop this Add silently
		return nil
	}
	m.latest[op.ID] = op
	if op.Status != operation.StatusReady {
		m.mu.Unlock()
		return m.forward(op)
	}
	verdict, reason := m.opts.Policy(op)
	switch verdict {
	case Deny:
		m.mu.Unlock()
		m.emit(TerminalSnapshot(op, operation.StatusCanceled, reason))
		return nil
	case Ask:
		if m.opts.Ask == nil {
			break
		}
		ctx, cancel := context.WithCancel(m.ctx)
		m.held[op.ID] = &heldOp{op: op, cancel: cancel}
		m.mu.Unlock()
		m.opts.Ask(ctx, op)
		return nil
	}
	m.mu.Unlock()
	return m.forward(op)
}

func (m *Manager) forward(op operation.Operation) error {
	if err := m.inner.Add(op); err != nil {
		return err
	}
	if m.opts.OpTimeout > 0 {
		id, limit := op.ID, m.opts.OpTimeout
		timer := time.AfterFunc(limit, func() {
			_ = m.inner.Cancel(id, fmt.Sprintf("command exceeded the %s time limit", limit))
		})
		m.mu.Lock()
		m.timers[id] = timer
		m.mu.Unlock()
	}
	return nil
}

// Decide resolves a held op: forward it, or answer it canceled with reason.
// It is a no-op for ops that are not held.
func (m *Manager) Decide(id operation.ID, allow bool, reason string) {
	m.mu.Lock()
	held := m.held[id]
	if held == nil {
		m.mu.Unlock()
		return
	}
	delete(m.held, id)
	m.mu.Unlock()
	held.cancel()
	if allow {
		if err := m.forward(held.op); err != nil {
			m.emit(TerminalSnapshot(held.op, operation.StatusFailed, "start operation: "+err.Error()))
		}
		return
	}
	m.emit(TerminalSnapshot(held.op, operation.StatusCanceled, reason))
}

// Held reports whether id awaits a decision.
func (m *Manager) Held(id operation.ID) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.held[id] != nil
}

// Cancel implements operation.Manager.
func (m *Manager) Cancel(id operation.ID, reason string) error {
	if m.Held(id) {
		m.Decide(id, false, "canceled before approval: "+reason)
		return nil
	}
	m.mu.Lock()
	latest, known := m.latest[id]
	m.mu.Unlock()
	if known && terminal(latest.Status) {
		return nil // already ended (possibly synthesized; the inner manager never saw it)
	}
	return m.inner.Cancel(id, reason)
}

// Updates implements operation.Manager.
func (m *Manager) Updates() <-chan operation.Operation { return m.out }

// Drained is closed after the inner manager finished every primitive and
// closed its Updates channel (operation/local_manager.go:113-115, 317-324).
func (m *Manager) Drained() <-chan struct{} { return m.drained }

func (m *Manager) emit(op operation.Operation) {
	m.mu.Lock()
	m.latest[op.ID] = op
	if terminal(op.Status) {
		if timer := m.timers[op.ID]; timer != nil {
			timer.Stop()
			delete(m.timers, op.ID)
		}
	}
	m.mu.Unlock()
	m.opts.Tap(op)
	m.updates.Push(op)
}

func (m *Manager) pump() {
	defer close(m.drained)
	for op := range m.inner.Updates() {
		m.emit(op)
	}
}

func (m *Manager) feed() {
	for {
		op, ok := m.updates.Pop(m.ctx)
		if !ok {
			return
		}
		select {
		case m.out <- op:
		case <-m.ctx.Done():
			return
		}
	}
}

func terminal(status operation.Status) bool {
	switch status {
	case operation.StatusCompleted, operation.StatusFailed, operation.StatusCanceled:
		return true
	}
	return false
}

// TerminalSnapshot builds a terminal state the built-in translators render.
// The harness keeps its constructors unexported (operation/shell.go:599-621),
// so the exported ShellState is edited directly.
func TerminalSnapshot(op operation.Operation, status operation.Status, reason string) operation.Operation {
	op.Status = status
	if op.Type != operation.TypeShell {
		return op
	}
	state, err := operation.DecodeShellState(op)
	if err != nil {
		return op
	}
	state.Phase = ""
	state.TerminalError = reason
	state.ErrorTruncated = false
	encoded, err := json.Marshal(state)
	if err != nil {
		return op
	}
	op.State = encoded
	return op
}
