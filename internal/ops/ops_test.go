package ops

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/operation"
)

// fakeInner models LocalOperationManager's duplicate-Add behaviour
// (local_manager.go:133-137): a known ID is accepted silently.
type fakeInner struct {
	mu       sync.Mutex
	accepted map[operation.ID]bool
	adds     int
	cancels  []operation.ID
	updates  chan operation.Operation
}

func newFakeInner() *fakeInner {
	return &fakeInner{accepted: map[operation.ID]bool{}, updates: make(chan operation.Operation, 16)}
}

func (f *fakeInner) Add(op operation.Operation) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.accepted[op.ID] {
		return nil
	}
	f.accepted[op.ID] = true
	f.adds++
	return nil
}

func (f *fakeInner) Cancel(id operation.ID, _ string) error {
	f.mu.Lock()
	f.cancels = append(f.cancels, id)
	f.mu.Unlock()
	return nil
}

func (f *fakeInner) Updates() <-chan operation.Operation { return f.updates }

func shellOp(t *testing.T, id string) operation.Operation {
	t.Helper()
	spec, err := operation.NewShellSpec(operation.ShellInput{Command: "true", Shell: "/bin/sh", Directory: "/"}, t.TempDir(), 4096)
	if err != nil {
		t.Fatal(err)
	}
	return operation.Operation{ID: operation.ID(id), Type: spec.Type, Version: spec.Version, Status: operation.StatusReady, State: spec.State, MaxOutputLength: spec.MaxOutputLength}
}

func next(t *testing.T, m *Manager) operation.Operation {
	t.Helper()
	select {
	case op := <-m.Updates():
		return op
	case <-time.After(2 * time.Second):
		t.Fatal("no update")
		return operation.Operation{}
	}
}

func none(t *testing.T, m *Manager) {
	t.Helper()
	select {
	case op := <-m.Updates():
		t.Fatalf("unexpected update %+v", op)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestDuplicateAddReemitsLatestSnapshot(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	inner := newFakeInner()
	m := New(ctx, inner, Options{})
	op := shellOp(t, "op1")
	if err := m.Add(op); err != nil {
		t.Fatal(err)
	}
	done := op
	done.Status = operation.StatusCompleted
	inner.updates <- done
	if got := next(t, m); got.Status != operation.StatusCompleted {
		t.Fatalf("got %s", got.Status)
	}
	// A new coordinator generation re-Adds the op (e.g. its predecessor died
	// holding the terminal update). The inner manager would stay silent.
	if err := m.Add(op); err != nil {
		t.Fatal(err)
	}
	if got := next(t, m); got.ID != "op1" || got.Status != operation.StatusCompleted {
		t.Fatalf("re-emit = %+v", got)
	}
	if inner.adds != 1 {
		t.Fatalf("inner adds = %d, want 1", inner.adds)
	}
}

func TestAskHoldsUntilDecidedAndCancelAnswersTerminal(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	inner := newFakeInner()
	asked := make(chan operation.ID, 4)
	m := New(ctx, inner, Options{
		Policy: func(operation.Operation) (Verdict, string) { return Ask, "" },
		Ask:    func(_ context.Context, op operation.Operation) { asked <- op.ID },
	})
	a, b := shellOp(t, "a"), shellOp(t, "b")
	_ = m.Add(a)
	_ = m.Add(b)
	<-asked
	<-asked
	_ = m.Add(a) // re-Add while held: silent, no second ask
	none(t, m)
	if len(asked) != 0 || inner.adds != 0 {
		t.Fatalf("held op started or re-asked")
	}
	m.Decide("a", true, "")
	if inner.adds != 1 {
		t.Fatalf("allowed op not forwarded")
	}
	if err := m.Cancel("b", "cancelled by the user"); err != nil {
		t.Fatal(err)
	}
	got := next(t, m)
	state, err := operation.DecodeShellState(got)
	if err != nil || got.Status != operation.StatusCanceled || state.TerminalError == "" {
		t.Fatalf("cancel of held op: %+v %+v %v", got, state, err)
	}
	if len(inner.cancels) != 0 {
		t.Fatalf("held op cancel reached inner manager")
	}
}

func TestDenyIsSynthesizedTerminal(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	inner := newFakeInner()
	m := New(ctx, inner, Options{Policy: func(operation.Operation) (Verdict, string) { return Deny, "not allowed" }})
	_ = m.Add(shellOp(t, "x"))
	got := next(t, m)
	state, _ := operation.DecodeShellState(got)
	if got.Status != operation.StatusCanceled || state.TerminalError != "not allowed" || inner.adds != 0 {
		t.Fatalf("deny: %+v %+v adds=%d", got, state, inner.adds)
	}
	_ = m.Cancel("x", "late")
	if len(inner.cancels) != 0 {
		t.Fatal("cancel of a synthesized terminal op must not reach the inner manager")
	}
}

func TestOpTimeoutCancelsForwardedOp(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	inner := newFakeInner()
	m := New(ctx, inner, Options{OpTimeout: 50 * time.Millisecond})
	_ = m.Add(shellOp(t, "slow"))
	time.Sleep(200 * time.Millisecond)
	inner.mu.Lock()
	defer inner.mu.Unlock()
	if len(inner.cancels) != 1 || inner.cancels[0] != "slow" {
		t.Fatalf("cancels = %v", inner.cancels)
	}
}

// A TERM-ignoring tool outlives inner.Cancel by the library's fixed 5s
// escalation; after OpTimeout + KillGrace the op's process group is killed.
func TestOpTimeoutKillsAfterGrace(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	inner := newFakeInner()
	killed := make(chan operation.ID, 4)
	m := New(ctx, inner, Options{OpTimeout: 30 * time.Millisecond, KillGrace: 60 * time.Millisecond, Kill: func(id operation.ID) { killed <- id }})
	start := time.Now()
	_ = m.Add(shellOp(t, "stubborn"))
	select {
	case id := <-killed:
		if id != "stubborn" {
			t.Fatalf("killed %q", id)
		}
		if took := time.Since(start); took < 90*time.Millisecond {
			t.Fatalf("killed after %s, before timeout + grace", took)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("op was never killed")
	}
}

func TestOpTimeoutDoesNotKillAnOpThatEnded(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	inner := newFakeInner()
	killed := make(chan operation.ID, 4)
	m := New(ctx, inner, Options{OpTimeout: 30 * time.Millisecond, KillGrace: 60 * time.Millisecond, Kill: func(id operation.ID) { killed <- id }})
	op := shellOp(t, "polite")
	_ = m.Add(op)
	// The op honours the cancel and ends within the grace.
	waitCancel := time.Now().Add(2 * time.Second)
	for {
		inner.mu.Lock()
		n := len(inner.cancels)
		inner.mu.Unlock()
		if n > 0 {
			break
		}
		if time.Now().After(waitCancel) {
			t.Fatal("no cancel")
		}
		time.Sleep(5 * time.Millisecond)
	}
	op.Status = operation.StatusCanceled
	inner.updates <- op
	next(t, m)
	select {
	case id := <-killed:
		t.Fatalf("killed %q after it ended", id)
	case <-time.After(150 * time.Millisecond):
	}
}
